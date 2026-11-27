# The JIT project

This is the plan of the JIT's next project: the architecture the engine
builds towards, the staged path to it, and the decisions taken on the way.
It is not how the JIT works today; that is
[internal/jit/README.md](../internal/jit/README.md)'s, which a stage's
parts move into as they land, while this document keeps what is ahead.
The items of [performance-plan.md](performance-plan.md) marked *(JIT
project)* are taken up by the stages below, each linked to the stage that
takes it, and not apart from them. Estimates of what has not been built
are marked *estimate*; every other figure was measured, and says how.

## Contents

- [The goals](#the-goals)
- [The evidence](#the-evidence)
- [Where the time goes](#where-the-time-goes)
- [The architecture](#the-architecture)
- [The stages](#the-stages)
- [The projection](#the-projection)
- [The decisions](#the-decisions)
- [The risks](#the-risks)
- [How this document is kept](#how-this-document-is-kept)

## The goals

The figures the goals start from, at `976e5cc1` (v0.45.1's code):

| | JIT | JIT off | Node | Node jitless |
|---|---:|---:|---:|---:|
| V8 composite, Ryzen 5 3600 | 7,883 | 3,109 | 39,779 | 2,342 |
| V8 composite, Apple M5 Max | 20,395 | 8,103 | 111,044 | 6,324 |
| Typescript, one run, ms | 1,968 on the spot, 1,713 in the background | 1,565 | 460 | 1,525 |
| Typescript's compiles in a run | 627, 466 ms, 0.74 ms each | | 544 Maglev and 65 TurboFan jobs, 446 ms on helper threads | |

The Ryzen's go-quickjs composites are benchmarks.md's, the median of
three fresh-process runs of `v8bench/matrix -suites score`; Node's are
`v8bench/external -engine node`, with `-arg --jitless` for jitless, on the
same machine. The M5 Max's are the maintainer's runs of the same score
mode on that machine (arm64). Typescript is Octane's, `v8bench -bench
octane -suite Typescript -mode fixed -n 1` with the runner's 32 MiB JIT
budget, and its compiles are `-jitreport`'s and Node's own traces.

1. **The V8 composite at least doubled:** 7,883 to 15,800 on the Ryzen,
   20,395 to 40,800 on the M5 Max. The composite is the geometric mean of
   eight suites, so the goal is a product of per-suite speedups of 256.
   It is an optimizing-tier goal: V8's own baseline, Sparkplug, measures
   1.38x Ignition on this suite, less than the JIT already has, and Maglev
   10.2x. In V8's terms 15,800 is about 5.2x Ignition, half of what
   Maglev alone gives.
2. **Typescript faster with the JIT than without** (1,968 ms and 1,713
   against 1,565). It is a tiering and latency goal: background compiles,
   a runtime goroutine that waits for almost nothing, deopts that land in
   native code, and fewer recompiles.
3. **Both tiers fast, and no interpreter regression.** The interpreter is
   1.33x jitless Node on the Ryzen and 1.28x on the M5 Max, but 0.82x on
   EarleyBoyer. Nothing here may make it slower: what the JIT adds to the
   interpreter goes in the feedback dispatcher, which a runtime without
   the JIT never runs, and every stage is gated on `placements` with the
   JIT off.
4. **A JIT-built binary with the JIT off no slower than the plain build.**
   It is level on the suites (the V8 suite -0.5%, Octane -0.7%, Are We
   Fast Yet +0.2%) but 0.7% slower on the micro-benchmarks' geometric
   mean, with string appends and BigInts 7 to 16% and generators 4 to 9%
   slower at both placements (`placements -micro`, plain against a
   `quickjs_jit` build run without `WithJIT`, Ryzen 5 3600).
5. **The JIT memory default relaxed** so that real programs run as they
   should with no option set. The 8 MiB default churns Typescript: 907
   functions' code dropped and 608 compiles refused in one run, and 2.7
   times the compiles over three runs. Every published figure is already
   measured at 32 MiB.
6. **Compile speed, per tier**, against V8 and the WebAssembly engine's
   optimizer
   ([wasm/internal/jit/opt/README.md](../wasm/internal/jit/opt/README.md),
   "The compile's cost"):

| Tier | Today | V8 | Target |
|---|---|---|---|
| baseline | none | Sparkplug 8.2 µs a function, Typescript's 692 in 5.7 ms | at most 0.25 µs a register instruction, about 10 µs for Typescript's mean function of 38; 0.5 µs is the kill gate |
| optimizing, a Typescript compile | 0.69 to 0.74 ms | Maglev 0.33 ms a job, on a worker | at most 0.40 ms, all phases |
| optimizing, the runtime goroutine's share in the background | about 0.3 ms a compile | Maglev 6 µs, TurboFan 15 µs | at most 30 µs |
| Typescript's compile CPU in a run | 466 ms, 156 of 627 compiles after deopts | 446 ms over 609 jobs | at most 250 ms, recompiles after deopts at most 10% of compiles |
| a run's compiles' garbage, Typescript | 77 MB, 287,000 objects | | at most 20 MB; the wasm optimizer makes 5 objects a warm compile |

A stage that buys run time with compile time states the trade and makes
its case; AGENTS.md's rule applies, that a phase more than a tenth slower
is a regression unless the maintainer accepts it.

## The evidence

The figures come from six bodies of measurement, made at v0.45.1 in
November 2026. Each figure below names what it measured; this list says
how.

- **Profiles of the engine,** on a Ryzen 5 3600 under Windows, over the
  V8 suite, Octane, Are We Fast Yet and ssr, with the JIT and without:
  CPU profiles (`v8bench -cpuprofile`) joined with the JIT's code
  (`-jitcode`, `jitprof`) for native code's time by what each stretch was
  emitted for; `-jitreport` and the tier trace (`-jittier`) for compiles,
  deopts and exits; Typescript's interpreted instructions split by why
  they ran interpreted; the live heap and RSS at JIT budgets of 8, 32, 64
  MiB and none. A time in this document is this machine's unless it
  comes from the studies on the Ryzen 9 9950X3D2 below, whose figures
  are mostly shares and ratios.
- **V8's tiers:** Node 26 (V8 14.6) over the same suites with its tiers
  turned off one at a time by V8's flags, its compile and deopt traces,
  and V8 14.6's source for its thresholds and bounds; ratios measured on
  a Ryzen 9 9950X3D2.
- **Code quality against V8:** the hot path of each V8-suite benchmark,
  ours and TurboFan's, for the same work, counted in instructions and
  timed an iteration or a call, on amd64; each of our sampled
  instructions assigned to the optimization that would remove it.
- **Our compiler and tiers:** `-jitcompile` per phase on Typescript and
  the other Octane programs, the native handlers' code read for what a
  baseline would keep, the budgets and the memory flush tried at other
  settings, on the Ryzen 9 9950X3D2.
- **The memory model:** a prototype of an engine-managed heap in pure Go
  (NaN-boxed values, objects in mapped memory, a bump nursery and a
  copying collector), micro-benchmarks of a property chain's loads and of
  reference stores in both heaps, and the pools' refills profiled, on the
  Ryzen 9 9950X3D2.
- **Other engines' designs:** the published designs and measurements of
  JavaScriptCore, SpiderMonkey, HotSpot, CPython and copy-and-patch
  compilation, and this repository's WebAssembly engine, whose tiers
  (wasm/internal/jit/README.md, "Tiers") already compile in the
  background and reach 0.97x Node in steady state and on first runs.

## Where the time goes

**The steady state** of the V8 suite is native code and Go. Native code is
98% of Richards, Crypto and NavierStokes, 84% of DeltaBlue, 61% of
RayTrace and EarleyBoyer, 32% of Splay and 18% of RegExp; the interpreter
is 0.2 to 2% (RegExp 10%), and compiling under 1%. Inside native code,
property access is 30 to 55% of the samples on five suites and calls 15
to 30%, and Crypto spends 46% converting between int32 and double. Beside
it, the pools' refills take 9 to 34% of DeltaBlue, RayTrace, EarleyBoyer
and Splay; Go making closures and cells takes 52% of Earley; Go's side of
the regular expression leaves takes 66% of RegExp, and string
concatenation 20% of Splay. With cores to spare, 89 to 100% of each
suite's distance to Node is the design, not Go.

**Typescript's one run** loses to compiling (29%, 466 ms, 156 of its 627
compiles after deopts); to the interpreter while code exists or existed
(65% of its interpreted instructions: 192 deopts, and 2,098 runs left with
no entry to go on at, 1,274 of them at `load_undef`); to the interpreter
while it warms up (29%); and to Go's work for native code (11%). In the
background the runtime's goroutine still spends 11% of the run lowering,
snapshotting feedback and installing, about 0.3 ms a compile, where V8's
main thread spends 6 to 15 µs. A synchronous V8 loses 45% of its
Typescript score, and Sparkplug alone gives V8 1.75x Ignition on it.

**The code against TurboFan's,** for the same hot paths and the same work:

| Benchmark | hot path | ours | V8 | instructions | time |
|---|---|---:|---:|---:|---:|
| Richards | `schedule`'s held path, an iteration | ~200, 77 memory | 27, 9 | 7.4x | 8.4x |
| DeltaBlue | the plan loop, one EqualityConstraint | ~620, ~140 guards | 22, 6 | 28x | 18x |
| Crypto | `am3`'s inner loop, an iteration | 227 | 64 (peeled) | 3.5x | 5.6x |
| RayTrace | a `Vector.subtract` call | ~490 | ~130 | 3.8x | 3.6x |
| EarleyBoyer | a `one_way_unify1` call | ~770 | ~155 | ~5x | 3.9x |
| Splay | a `splay_` iteration | ~191 | ~85 | 2.2x | 1.8x, memory-bound on both |
| NavierStokes | `lin_solve`'s inner loop, an iteration | 181 | 43 | 4.2x | 2.3x, latency-bound |

The differences fall into classes. Each class's gain to the composite
alone, closing 60%, 75% or 90% of its measured gap (low, middle, high):

| Class | composite | where it is |
|---|---|---|
| int32 and float64 in registers, from operand feedback | +8.7 / +13.5 / +20.6% | Crypto 55-68% of native time, NavierStokes 30-38% (mostly double indices), Richards 7-10% |
| constants and guards through dependencies | +3.3 / +5.0 / +7.1% | RayTrace 20-26%, DeltaBlue 14-19%, Richards 12-17%, EarleyBoyer 12-15% |
| property access: values out of the object, the cell, 16-byte Values | +1.9 / +3.0 / +4.4% | Richards 11-16% |
| calls V8 inlines and we call | +1.9 / +2.8 / +3.8% | DeltaBlue 19-21%, Richards 6-7%, others 1-4% |
| the call sequence (~115 instructions against 40 to 60) | +1.7 / +2.5 / +3.5% | EarleyBoyer 10-13%, RayTrace 7-10%, Splay 8-11% |
| loop-invariant checks and loads, OSR phis | +1.4 / +2.0 / +2.8% | DeltaBlue 14-16% once inlined |
| allocation's native part: pool takes, property adds | +1.2 / +1.7 / +2.3% | RayTrace 13-17%, Splay 11-14%, EarleyBoyer 9-11% |
| elements kinds | +0.9 / +1.5 / +2.3% | NavierStokes 6-9% |
| redundant guards across the CFG | +0.7 / +1.2 / +2.0% | Splay 5-7% |
| branches, booleans, constants, copies | +0.5 / +0.9 / +1.3% | Richards 2-4% |
| barrier tests, keep and kept | +0.5 / +0.7 / +1.1% | Splay 8-9% |
| numbers as doubles in fields | +0.4 / +0.7 / +1.0% | Richards 5-8% |
| spills and moves | +0.3 / +0.5 / +0.7% | amd64 only |

Together the classes remove 45 to 87% of each suite's native samples,
under the time ratio to V8 everywhere but the latency-bound loops. Four
findings cut across them:

- **Several entries in one compile defeat invariance.** `this`, Crypto's
  arrays and NavierStokes' `x` and `x0` are phis of the start's value and
  the OSR entries' frame loads, so no pass can know them invariant.
- **Facts die at every merge, loop header and poll.** `knownShapes` keeps
  shapes only along a block's single-predecessor chain, and ssa has no
  load elimination, store forwarding, loop-invariant code motion or
  peeling.
- **Seven general registers on amd64,** against V8's about twelve.
- **A call is three ladders, nine frame-record stores, two checks and an
  exit flag.**

No class is large alone. The largest, int32 code from operand feedback,
is stage 7's; the rest are a wave of smaller changes, W1 to W9, which the
stages below place.

## The architecture

Three tiers share one frame, the register window, and one feedback:

- the **interpreter**, the native handlers, plain in runtimes without the
  JIT and feedback in runtimes with it (the Go table elsewhere);
- a **baseline** tier made from the same handlers' code, copied and
  patched per function, which never deoptimizes;
- the **optimizing** tier, ssa, mir and the lowering, as now.

The optimizing tier compiles in the background by default, as the
WebAssembly engine's does. It speculates on operand types as well as on
the caches, folds constants through shape and field dependencies, inlines
polymorphic calls, compiles OSR entries apart and peels loops, and carries
facts and loads across the CFG. It reaches what it does not inline
through a middle level of machine-code routines before Go, and makes
closures, cells and objects natively from pools, objects with their final
shapes. Its deopts resume in baseline code. Object values sit in the
object, and arrays of numbers are stored as numbers. An engine-managed
heap is the one part that stays a decision, made on a spike's numbers.
Nothing in today's design is held fixed: each part is kept or changed on
the evidence.

### Tiers, and how code moves between them

| Tier | What it is | Entered at | Leaves for |
|---|---|---|---|
| interpreter | the native handlers, feedback variant in JIT runtimes | a function's first calls | baseline, at 8 invocations of its length |
| baseline | per function: the feedback variant's fast paths copied from stencils, operands patched in, dispatch removed; slow paths shared routines and Go, as the handlers' are | 8 x length, V8's point of feedback allocation | optimizing, at 400 x length |
| optimizing | ssa, mir, speculating | 400 x length (`jitInvocationsForMaglev`, V8's Maglev point) | baseline, at a deopt |

- **Counting** stays as it is (internal/jit/README.md, "Tier-up and
  on-stack replacement"): returns spend a function's length, back edges
  their distance. Baseline code counts the same way, inline, and leaves
  to tier up as the WebAssembly baseline does (`ExitTierUp`), at its entry
  and at its poll. `jitBudgetForFeedback`'s 940 becomes V8 14.6's unit, 8
  invocations of the function's length, with the baseline tier (stage 5)
  and not before: in V8 that point starts a cheap baseline compile, and
  without one it would only start the optimizing tier's on-stack compiles
  sooner, synchronously, which the earlier trial of V8's budgets without
  its tiers showed loses time.
- **Deopts resume in baseline code.** Baseline code has an entry at every
  register pc, through a pc map of 4 bytes an instruction, as V8's offset
  table is. A deopt writes the window as today, and Go goes on in baseline
  code at the exit's pc, not in the interpreter for the rest of the
  invocation. A host exit at a pc where optimized code has no entry goes
  on in baseline code too, not in the interpreter to the next back edge,
  which removes Typescript's 2,098 runs with no entry at their root.
  Inlined frames an exit materializes each resume in their function's
  baseline code where it has some.
- **OSR.** The interpreter's and the baseline's frames are the same, so
  OSR between them is a jump, as JavaScriptCore's LLInt and Baseline share
  an ABI at every instruction. Into the optimizing tier, **an OSR entry is
  a compile of its own**, entered at its loop header with the frame's
  values as parameters, as V8 compiles OSR apart; a function's start code
  then has one entry, and the phis of the start's values and the entries'
  loads go. Today's multi-entry form stays only where an OSR compile is
  refused. The cost is extra compiles: V8 makes 4 of Richards' 52 compiles
  for OSR, and 2 of Typescript's 609.
- **Sharing.** Baseline code is per template (`closureCode`), shared by
  every closure and every realm: it is position-independent and reads
  everything from the context, as the handlers do. V8 compiles per
  SharedFunctionInfo and JavaScriptCore compiles unlinked baseline code
  alike.
- **The WebAssembly engine's tiering, reused:** the tier-up exit with the
  countdown set to the maximum once queued; its workers, queue and one-P
  policy (below, "Background compilation as the default"); placement
  apart (`PlaceRawApart`), on pages no code ran from; publishing through
  the function's link; replaced code kept for the frames begun in it; the
  budget pass that drops code by epochs. What differs is that a JS compile
  reads feedback from the runtime's heap, which the snapshot answers.
- **The debugger, `with` and direct eval** stay interpreted, as now.
  Hardened darwin processes, with no executable memory, run no baseline
  code, as they run no JIT.

**Why.** A baseline starts code natively and cheaply, and gives deopts and
code not yet optimized somewhere to run other than the interpreter. What
it would take of the interpreter's time is 34 to 98% of a suite's
interpreted time while code warms up, and 0 to 88% after deopts and exits;
on Typescript a baseline 1.5 to 2x the interpreter's speed recovers 150
to 230 ms of a 1,713 ms run. Over an assembly interpreter like ours,
SpiderMonkey's Baseline Compiler is 1.5x and 1.75x its Baseline
Interpreter with the same caches and frames, JavaScriptCore's Baseline
2.3x its LLInt a bytecode, and Sparkplug 1.75x Ignition on Typescript.
The handlers spend about half of `add`'s 28 instructions on dispatch and
decoding, and an estimated 15 to 30% of a property read's or a call's.

**Rejected:**

- *No baseline, only a later optimizing tier.* It made ssr's first
  renders slower when tried (performance-plan.md, Rejected, "A later
  tier-up").
- *A second, cheaper optimizing configuration.* Removing the optimizer's
  rounds saved 0 to 13% of compile time and grew code 15 to 30%; it would
  be a second compiler to keep correct, and the baseline covers the cheap
  end.
- *A TurboFan-class tier above.* TurboFan adds 1.3x over Maglev on the V8
  suite and nothing on Typescript. The optimizing tier is Maglev-shaped
  already; its gap is code quality, not a missing tier.
- *A tracing tier.* It is bimodal on branchy code, which is why
  SpiderMonkey removed its tracing JIT.
- *A baseline made by an assembler linked into package vm.* Stencils
  compile faster, keep the JIT's assemblers out of vm's text and the
  layout lock, and come from the emitter that writes the handlers.

With a baseline under it the optimizing tier's threshold is free to rise:
JavaScriptCore, SpiderMonkey and HotSpot keep their optimizers 10 to 25
times above their baselines. It starts at V8's 400, which V8 keeps for
Maglev with Sparkplug below, and moves only on measurement (D4).

### Feedback

The caches stay the feedback, and three things join them, kept in the
feedback variant and in baseline code alone:

1. **Operand-type feedback.** Each arithmetic, comparison and bitwise site
   gets a byte, a lattice its feedback handler ORs into: none,
   signed-int32, number, number-or-oddball, BigInt64, BigInt, string, any,
   V8's BinaryOp and CompareOp hints. It lives in the function's feedback
   slots, reached through the register already reserved for them (`fv`,
   R14 on amd64, R15 on arm64; internal/jit/README.md, "Feedback"). The
   builder speculates exactly what was seen; a site that saw nothing is
   left to Go, with no invalidation, V8's soft deopt; a site that saw
   everything is built generic. This replaces learning strings, BigInts
   and objects by exits (`StringAdd`, `regBigOf`'s marks, `Generic`), so
   that the first compile is right by construction.
2. **Deopt memory on the template:** a delay bit, V8's kDelayMaglev, and
   a count of deopts that backs the budget off -- with the baseline tier
   (stage 5), not before (below).
3. **Allocation-site survival:** a count a pool's refill keeps, which
   sizes the next refill, pretenuring's counterpart.

**Why.** V8's few deopts are not a low count: it deopts as often as we do
on Typescript, 183 against 192. They come from first compiles that
speculate only what was seen, and from deopts that cost little. Crypto's
46% in int32 and double conversions is code compiled without knowing its
operands are int32s. The plain handlers stay byte-identical
(`TestRegNativePlainSameWithJIT`), and the byte costs only a JIT
runtime's warm-up.

**Rejected:** operand feedback in the plain handlers, which would slow
every runtime without the JIT; feedback from the Go table, which counts
nothing for the JIT (on other architectures and under `QJS_NONATIVE` the
JIT speculates numbers as now). CacheIR-style feedback, every cache case
one data form all tiers run, and trial inlining's per-site feedback are
worth having but are the next project's (D12): CacheIR is a
re-architecture of the caches, and trial inlining needs the baseline
first. The snapshot takes CacheIR's cheap half, a copy of the caches'
data.

### Deoptimization discipline

`jitMaxDeopts`'s refusal after 64 deopts goes; V8 has none for JavaScript
(D5). In its place:

- **Per site:** the `Generic` mark, V8's disallowed speculation; the
  operand lattice widening; property cases widening to megamorphic.
  JavaScriptCore's exit flags are the same idea.
- **Per function, until the baseline tier:** a deopt resets the budget,
  as now. The cap's work of ending a deopt loop is done by the per-site
  marks: each deopt widens what it failed on, so a function's deopts are
  bounded by its sites.
- **Per function, with the baseline tier (stage 5):** V8's and
  JavaScriptCore's tier-up policy. A deopt of code younger than 600
  invocations sets the template's delay bit, and its next compile waits
  for 1,100 invocations' worth, as V8's does; each further deopt doubles
  the wait, as JavaScriptCore's 2^R does, up to 2^6. A cache's change
  during warm-up raises what is left to 500 x length from the first runs
  on, which keeps RayTrace's `testIntersection` from compiling at its
  166th invocation on feedback still changing (performance-plan.md, "The
  tier-up's budgets"); and the feedback point becomes 8 x length.
- **Why that waits for a good baseline.** Each of those delays an
  optimizing compile, and V8 can afford them because the code waits in
  Sparkplug, natively. Here it would wait in the interpreter, and the
  trial of V8's budgets without its tiers lost time (RayTrace 1.066,
  Octane -n 20 0.986). So they switch on only once the baseline passes
  its gate below -- its compile speed, and its code at least 1.5x the
  interpreter's on the first runs -- each timed against the policy
  before it.
- With the baseline, a deopt costs a frame conversion and the function
  runs on natively; only the recompile remains, in the background.
- Refusal is for bailouts alone: a compiler panic, `abi.MaxSlot`, the
  debugger.

Half of Typescript's compile time is recompiles after deopts, at 1.0 ms
each against 0.25 for a first compile. Operand feedback, the delay and
back-off, resuming in baseline code and the background together cut
them; the target is 10% of compiles or fewer, from 25%. Measured on the
Ryzen 9 9950X3D2: V8's delay bit alone cut 5 to 7% of compile time, and a
budget of 3,000 halved the deopt churn.

### The baseline compiler: copy and patch

Offline, nativegen emits each handler's fast path a second time, as a
**stencil**: machine code with holes.

- Operand decoding becomes a hole for the window's displacement;
  constants become immediate holes; branch targets rel32 holes, or on
  arm64 imm19 and imm26 fields. arm64 has stencil variants for window
  offsets past imm12's scaled 32 KB.
- The dispatch, four instructions and an indirect jump, becomes a fall
  through.
- Rare paths become jumps to shared routines in Go's text (below), which
  keeps a stencil under 70 bytes an instruction.

A compile copies the stencils in order, patches the holes, writes the pc
map and places the result in the JIT's arena in batches, as V8 batches 4
KB of estimated code and `PlaceRawApart` places a batch. The emitter is
written once for three uses, plain, feedback and baseline; the stencils
are generated files beside `native_handlers_*.s`, kept current by the
same staleness test. The baseline's call and return need a frame record
that knows the caller's tier, so they are a variant
(`nativegen/variant.go`), and plain's call and return stay byte-identical.

**Why.** Compile speed: copy-and-patch beats V8's Liftoff 4.9 to 6.5x in
its published measurements, and a published stencil baseline for Lua
compiles 19 million bytecodes a second. The target of 0.25 µs an
instruction, about V8's 8 µs a function, is half the kill gate; all of
Typescript would compile in about 17 ms against the optimizing tier's 444
(*estimate*). Package vm links no assembler for it, so the layout lock
sees only data.

**Rejected:** an emitter run inside vm, slower to compile and linking
internal/jit into vm's hot range; CPython's micro-op traces, finer units
than the interpreter's that exited often and took three releases to beat
CPython's own interpreter.

### Calls: a middle level, Go called directly, the call convention

V8 has three levels: inline code, machine-code builtins and the C++
runtime. We have two, inline code and Go. The design adds the middle:
**routines** nativegen generates into Go's text, each the native
handlers' fast path for an operation with a register descriptor of its
own:

- the property probe, monomorphic, polymorphic and the megamorphic cache;
- keyed and element access;
- string comparison and short concatenation;
- a call's set-up for an interpreted or baseline callee;
- the pool's take and refill swap;
- `instanceof`, `in`, `typeof`.

Native code and baseline code jump to them with the return address in a
register, as native code calls everything now; a routine reaches Go only
on its miss, as a builtin reaches CEntry. The fixed registers that
performance-plan.md's item on `abi.GoStub`'s operands asks for become the
routines' descriptors, which we define, not a constraint on Go's stubs,
where they were measured at well under 0.5%.

**Go called in the direct form.** The JIT's stubs call a function's
ABIInternal entry through a func value, as `nativeCallH` does (on amd64
the arguments in AX, BX, CX and DI, g in R14, X15 zeroed), removing the
ABI0 wrapper and every argument's round trip through the stack.
`TestRegNativeCallHConvention` keeps it honest on Go 1.24 and the newest
Go.

**The native call convention (W4).** A call is about 115 instructions
against V8's 40 to 60: three ladders (the method's shape, the function's
identity storing a case index, the link by that index), two words an
argument, about nine frame-record stores, a depth check and a frame-room
check, and an exit flag tested after every return. It becomes one
dispatch a polymorphic call, the shape ladder yielding the link;
one-word arguments where the type is known; a frame record of two or
three stores; an exit reported by returning to a second address; and the
frame-room check once at entry for the largest inlined window. It shares
the baseline's frame record, one design with a variant for the baseline.
It cannot fix the indirect return's mispredictions, the price of never
executing CALL and RET.

**Native calls of baseline and interpreted callees,** without Go. They
make callees on a budget of their own affordable, which was tried and
made Are We Fast Yet 15% slower because those calls go through Go; the
native generic construction (performance-plan.md, "A construction Go
makes from native code") and calls of typed leaves belong here.

**Why.** The crossing into Go is 0.1 to 3% of the suites; what costs is
the work behind it that V8 does in generated code: a script call through
Go at about 120 ns (`jitRegGoCall`), Typescript's 5% in script calls Go
makes for native code and 3% in keyed reads and writes, a fresh class's
construction at about 90 ns. The routines also shrink compiles: the 65
kinds of cold code inlined at each site, about 23% of the code, become
jumps to shared routines.

**Rejected:** registering JIT code with Go's runtime through its module
data, which works only through linknamed runtime internals; a native
stack with real CALL and RET, worth 0.9 to 1.5 ns a return at the price
of switching stacks at every call of Go, rejected before with nothing new
for it.

### Closures and cells made natively

Native code makes every closure, captured cell and arguments object that
its frame or an inlined frame creates, from the pools `ClosureSite` and
`CellsPool` already use. The level a native callee runs at is in its
context, found in constant time, which replaces `jitRegFrameOp` and
`jitRegLevelAt`'s walk.

**Why.** Earley spends 52% in `jitGoFast` and `jitRegFrameOp`; the levels
found number 7,854,694 for 987 calls, and are why the JIT runs Earley
slower than the interpreter (5.11 ms a run against 4.45; Node 0.21). It
is EarleyBoyer's 12x gap to Node, the widest after DeltaBlue's, and V8
makes the closure and its context inline. Caching the walk alone was
rejected: it removes `jitRegLevelAt`'s 7% and leaves Go making the
objects.

### Allocation in the Go heap

1. **Folded takes.** A block's k allocations test their pools' counts
   once and take k objects, V8's allocation folding mapped onto pools.
2. **Bare blocks by size class, the header written at the take.** A pool
   holds zeroed blocks of a size class, not one site's made objects;
   native code writes the shape, the prototype and the fields as it
   takes, under the marking rule. One pool serves every site of its size,
   so refills come less often, each mallocgc alone.
3. **Refill ahead** (*estimate*, not prototyped). A helper goroutine fills
   a size class's next buffer while native code drains the current one,
   and the refill is a pointer swap, so mallocgc and its assists run on
   another P. At GOMAXPROCS 1, or with no P idle, the refill is made on
   the spot, under the background compiles' policy. It is a spike, landed
   only if it wins (D8).
4. **Refill size by survival:** a site whose objects survive gets larger
   refills, pretenuring's place.
5. **Allocation with the final shape (W8).** A constructor's or a
   literal's site allocates with the shape it ends with, as V8's slack
   tracking and initial map do: after a few constructions the construct
   path records the final shape, `this.x = ...` in an initializer is a
   store rather than a property add of 37 to 45 instructions, and the
   take writes the final shape. A `new Vector` in RayTrace is a pool take
   and three adds, about 156 instructions, against V8's one folded bump;
   its property adds are 11.6% of its analysed samples.
6. Escape analysis also removes the temporaries an inlined callee
   consumes.

**Why.** Refills are 9 to 34% of the four allocating suites, and 62 to
82% of a refill is mallocgc itself, which folding and stubs cannot go
below on the runtime's thread: hence the helper, and why the engine heap
is the only design that removes mallocgc. Item 2's ceiling is the rest of
a refill, 18 to 38%: Splay about 7%, RayTrace 5%, EarleyBoyer 7%. The
helper is worth up to the 17 to 24% inside mallocgc on Splay, EarleyBoyer
and RayTrace, with cores to spare only.

**Rejected:** slabs, where one survivor keeps its 63 neighbours, 4 to 6
times the memory; a nursery inside the Go heap, which moves objects, the
hardest part of the engine heap for part of its payoff.

### Values and the object model

| Step | What | Decision | Evidence |
|---|---|---|---|
| c1 | in-object values: a shape guarantees its first k values sit in the object's own allocation at fixed offsets, so a shape guard implies the address | build | a dependent load fewer a read: 1.43 to 0.72 ns a hop in L1, 3.6 to 2.5 in L2 (the prototype's micro-benchmark); property access is 30-55% of native samples |
| deps | shape and global-cell dependencies: code compiled against a stable shape (a prototype's, a constructor's, a global object's) registers on it, a transition invalidates the code at once, and the guard is not emitted | build | V8's compilation dependencies and stable maps; shapes live long, so code holding them strongly meets no weak-reference cap |
| W1 | constants through dependencies: fields written once, the shapes a field holds, constant globals, constant call targets; `Direction.FORWARD`, `sc_Pair.prototype`, `this.initialize` become constants, `instanceof` walks against a constant, and the identity check before an inlined call goes. Watched slots stay out of the caches' fast entries, so a store to one goes the slow way and invalidates, and the interpreter's store fast path is unchanged | build, with deps | RayTrace 20-26% of native time, DeltaBlue 14-19%, EarleyBoyer 12-15%; +1.7 / +2.8 / +4.6%; compiles 0-2% cheaper |
| c2 | elements kinds: int32 (4-byte words) and double (8-byte), noscan, then tagged `[]Value`, each array's transitions one way; with int32 speculation, indices and arithmetic as int32s in registers | build, gated on the interpreter's array micro-benchmarks (D6) | 55-68% of Crypto's native time and 30-38% of NavierStokes', mostly double indices; a full collection over 256 MiB of numbers 30 ms as Values, 1.3 ms as noscan words |
| W9 | int32 fields, a shape's field representation, which needs a Value with an int32 form | with the engine heap's decision | Richards' `state`, `count`, DeltaBlue's `mark`, `value`; +0.6 / +1.3 / +3.0% |
| c3 | bare pools, the header at the take | build (above) | |
| c4-c6 | slabs, a Go-heap nursery, an off-heap shape store | no | |
| engine heap | 8-byte NaN-boxed values; objects, arrays and cells in mapped memory; a bump nursery and a generational barrier inline; our own non-moving collector; collection deferred while an unaudited Go frame is on the stack | a decision after a spike (stage 10, D1) | +13-15% of the composite with cores to spare, about +40% at GOMAXPROCS 1, 0-3% on Typescript; 9 to 18 months (*estimate*) |

The int32 kind changes an array's storage, not a Value. The tagged-int
experiment put ints in every Value and traded Crypto for floats; this
keeps Values as they are and does what V8's Smi and double elements do.
An element the interpreter reads converts on the way out, so c2's gate is
the interpreter's array micro-benchmarks at two placements, none slower.

### The engine heap and its collector

**Why it is a decision, not a step.** The memory prototype shows an
engine heap is sound in pure Go; it helps the interpreter as much as the
JIT, and is the only design that removes the memory model's caps:
mallocgc, no nursery, the barrier's call while Go marks, and 16-byte
Values. But its payoff with cores to spare is about an eighth of the
composite, it touches every layer, and it brings use-after-free as a new
class of bug. The path does everything that pays in the Go heap first and
decides on a spike over Splay, EarleyBoyer, RayTrace and Havlak with the
JIT, once the projection shows how far the composite goal still is.

**The collector: StickyImmix,** the Immix family's generational
mark-region form, non-moving by default, stopping the world per runtime.
Three facts about this engine decide it:

1. *Our collector cannot scan Go's stacks.* A built-in written in Go
   holds JavaScript references in Go locals, and Go moves its stacks and
   lets nothing read them. So either every built-in takes handle scopes,
   as V8's C++ does -- a rewrite of most of package vm's built-ins -- or
   collection happens only at safepoints with no unaudited Go frame on
   the stack: the interpreter's and the JIT's polls and allocation slow
   paths. A built-in that allocates mid-call grows the heap until it
   returns; built-ins converted to handles later may let collection
   happen inside them. A missed root then costs memory, never corruption.
   That needs a collector that tolerates roots it cannot update, which
   rules out one that must move every object.
2. *A runtime is single-threaded and owns its heap.* Stopping the world
   is stopping one goroutine, and a pause scales with one runtime's heap,
   not the process's. It needs no concurrent marking against a running
   mutator, the hardest part of V8's and JavaScriptCore's collectors.
   Incremental marking can come later if large heaps' pauses matter, as
   for server rendering.
3. *The JIT wants bump allocation and a cheap barrier.* Immix bumps a
   pointer through a block's free lines, as fast as a nursery, without
   copying. Sticky mark bits make it generational without moving: a young
   collection marks from the roots and the remembered set only, leaving
   old objects' marks set, and sweeps young objects' lines. The barrier is
   a check and a store on an old object's first write, a log bit, about
   the 0.66 ns a store the prototype's card mark took against Go's 1.5 to
   4.3 ns while marking. Fragmentation is met by evacuating
   opportunistically what nothing holds conservatively; objects with
   uncertain roots, cached `uintptr` addresses or handles stay pinned.

| Collector | Why not |
|---|---|
| generational copying, a semi-space nursery and a compacting old space, as V8's | the best throughput for short-lived objects, and the prototype was one; but every root must be precise, Go's built-ins included: handle scopes everywhere. StickyImmix keeps most of its allocation speed and young-collection gain without that |
| non-generational mark-sweep | about what Go's collector does now: every collection marks the whole heap, where Splay, EarleyBoyer and RayTrace lose their time |
| reference counting with a cycle collector | a barrier on every store, reads and writes dearer, and cycles are common in JavaScript |
| JavaScriptCore's Riptide | the closest proven design -- non-moving, sticky mark bits, conservative stack scanning -- but concurrent, which one mutator a heap does not need |

Its shape:

- **Spaces:** blocks of 32 KB in lines of 256 bytes, size-classed; a
  space of its own for large objects; a process-wide read-only space for
  shared immutable data, such as built-in templates and atoms.
- **Roots:** precise for the interpreter's register frames; conservative
  for native frames at first, precise maps if evacuation needs them;
  handles for Go objects; collection deferred while an unaudited Go frame
  is on the stack.
- **Barrier:** log-on-first-write for old objects, inline in native code
  and in the native handlers.
- **Weak semantics:** ephemeron marking for WeakMap and WeakSet; WeakRefs
  cleared at a collection's end; FinalizationRegistry callbacks queued as
  jobs.
- **Accounting:** external bytes count in the trigger and against
  `WithMemoryLimit`.
- **Marking:** single-threaded per runtime at first, parallel helpers
  later for large heaps.

Stage 10's spike builds this collector and measures its young
collections' cost and its fragmentation against the copying prototype's.

**Shared with the WebAssembly engine.** The WebAssembly engine is on a
par with Node on its programs and kernels except where it allocates GC
objects: docs/wasm.md's `gc` and `arrays` run at 10x and 8x Node's
time, and what is left there is Go's -- each object's allocation, the
collector's marking, the barrier while it marks -- though native code
takes its objects from pools and an allocation stub. The collector is
built to serve both engines:

- **A package of its own** (`internal/heap`), imported by package vm and
  by the WebAssembly engine, as `internal/atomicwait` is; the engines
  stay apart (`TestEnginesApart`).
- **Typed layouts as an object kind.** A WebAssembly struct or array has
  a layout fixed at validation, every field's kind known; the collector
  traces it from its type, with no shape to read.
- **Exact roots.** The WebAssembly engine's frames hold references only
  in their reference slots, and its tables and globals are known, so its
  roots are precise; its JIT already stores references only through a
  barrier routine of its own (`wasmNativeStoreRef`) and keeps none in a
  register across an exit, so moving it to the card mark is contained.
- **One mutator per heap.** A JavaScript runtime has one; a store's heap
  needs its calls serialized, or a safepoint handshake across the
  goroutines that may call into it. What the WebAssembly API promises is
  checked before the store's heap is designed.
- **Interop:** a WebAssembly struct seen from JavaScript and a JavaScript
  object passed as `externref` are references in one heap, not handles
  between two.

The JavaScript engine adopts it first, and the WebAssembly engine after
(D13): the heap matters far more to JavaScript, and little real-world
WebAssembly depends on the GC proposal yet. The collector is designed for
both from the start, so that the WebAssembly engine's adoption is a
follow-up, not a redesign.

**External arrays under the engine heap.** An ArrayBuffer's object lives
in the engine heap; its bytes stay where they are -- the host's shared
slice, a Go slice, a WebAssembly memory's store -- reached through a
handle whose data address is cached as a `uintptr`. The collector
releases the handle when it frees the object, and `Value.Bytes()` keeps
its aliasing promise. Detach, transfer, resize and `memory.grow` refresh
the cache. A SharedArrayBuffer has one store and a handle per runtime.
External bytes count in the trigger and against `WithMemoryLimit`, as V8
counts external memory.

**What the JIT loses and takes on,** on one condition: no Go pointer is
left in anything native code touches.

- *Gone,* since our collector runs only at our safepoints and never while
  native code runs: the marking rule whole (internal/jit/README.md, "The
  write barrier and the marking rule") -- `storeRef`'s call into Go while
  Go marks, the shading cases, `byMarking`'s second copy of a path and
  `genMakeMarking`, `TestJITSameWorkWhileMarking`'s breach list; the keep
  cells as the collector's view, `KeepLimit` and `mir.ErrKeeps`, and with
  a non-moving heap the reloads of references after calls; every Go
  pointer in native code, Go objects being reached by handle. In mir that
  is about 200 references to the marking machinery and 350 to keeps, in
  13 files, and ssa's keep pass goes with them.
- *Added:* a barrier of our own, generational, always on and inline, a
  few instructions at every reference store; root maps at safepoints,
  conservative scanning covering only values held in registers across one
  under a non-moving heap, precise maps at every exit, call of Go and
  allocation slow path under a moving one; and everything native code
  reaches moved too, strings and every heap value, or the marking rule
  and the keep cells stay for what is left. String *bytes* may stay
  outside behind handles, as an ArrayBuffer's do.
- *Size* (*estimate*): code 3 to 8% smaller, the barrier's slow paths,
  shading paths, second copies and per-iteration keep copies gone against
  1 to 2% for the card marks; compiles 2 to 6% cheaper; a moving
  collector's precise maps take about half of that back.

### Background compilation as the default

`WithJITBackground` becomes the default, `WithJITBackground(false)` the
opt-out (D3); built in stage 1, with the measurements in its status.

- **The one-P policy, the WebAssembly engine's**
  (wasm/internal/jit/opt/README.md, "In the background"): at GOMAXPROCS 1
  a compile is made on the spot, decided at each tier-up. Above that, a
  job no worker has started by the function's second check after it was
  queued is taken back and compiled on the spot (*estimate* at two
  checks), which bounds the worst case at today's synchronous behaviour
  for a host running one runtime per core with every core busy.
- **Workers:** at most a quarter of GOMAXPROCS, started on demand; the
  queue served most-called first, with HotSpot's stale-task drop, a job
  whose function went unused for an epoch dropped.
- **The runtime's goroutine** does 30 µs a compile, from about 0.3 ms:
  lowering moves to the worker (register code is immutable, and the hints
  are copied with it); the feedback snapshot becomes a copy of the
  caches' data by register pc, for the function and the callees it may
  inline, not answers to the builder's questions, so it cannot miss and
  the rebuild at install for `jitSnapMiss` goes, as SpiderMonkey's Warp
  works from a snapshot; placement happens apart, on the worker, batched;
  installation is a link store at the next check.
- **Validity at install:** a compile is thrown away if its function
  deoptimized meanwhile (`jitRegJob`'s count, as now) or a shape
  dependency broke, as V8's finalization commits dependencies or retries.
- **Determinism** is the tests' and the tools', not the default's: every
  engine offers a synchronous switch rather than deterministic installs.
  Tests keep spot compiles through `testConfig`; `QJS_JIT_STRESS`'s
  `background=K` installs at the Kth check; `compilediff native`,
  `-jitheld`, `-jitcompile` and `mir.Timing` force the spot;
  `placements -jitbackground` compares both modes, as now.

**Why.** Typescript runs 13% faster in the background today, and V8
loses 45% of its Typescript score without concurrent compiles; the steady
state does not care. What holds the default back today is one P: warm-up
1.5 to 4.8 times as long and Typescript 27% slower at GOMAXPROCS 1, which
the WebAssembly engine solved by compiling on the spot there. With the
baseline, a function waits in baseline code, not the interpreter.

**Rejected:** background compiles only once a baseline exists, since
Typescript gains now; compiles that yield, since Go cannot preempt native
code and a cooperative compile on the runtime's goroutine is a spot
compile in pieces, adding nothing over the take-back.

### The JIT's memory budget

A default of **64 MiB**, or an eighth of `WithMemoryLimit` where that is
smaller, as now (D2):

1. **What programs need.** Every workload measured fits 32 MiB but Octane
   run whole in one runtime, which peaks at 43.6 MiB and at 32 MiB is
   held by the flush with nothing dropped, at under 1%. Typescript alone
   needs 20.7 to 24.5 MiB, and a framework-sized application is several
   of those.
2. **The baseline adds code:** 4.7 to 10 MB for all of Typescript's
   functions (*estimate*), half of it warm, taking Typescript to 25 to 30
   MiB.
3. **It costs only what a program uses:** RSS is the same at 32, 64 and
   unlimited for every workload needing less than 32.
4. **One default for both engines:** the WebAssembly runtime's JIT budget
   is 64 MiB already (wasm/internal/jit/README.md, "Concurrency and
   memory").
5. **V8 has no budget,** only a code range of 64 to 512 MB and the heap's
   limit, with bytecode flushed after six collections unused.

The case for 32 MiB is many idle runtimes, each keeping its high water:
100 at 64 MiB is 6.4 GiB. The answer is that idle runtimes give memory
back, not a low ceiling:

- **Lowered programs,** 19 to 78% of what is held, are dropped after a
  compile unless the function is likely to compile again (warming after a
  deopt, or a learned recompile pending); every measured figure would
  about halve.
- **The idle runtime,** which flushes nothing today, looks to flush when
  `Eval`, a call or a job's run returns to the host, dropping what has
  gone unused for the flush age, at no API cost. A process-wide sweeper
  flushing runtimes idle for 30 seconds is left until hosts with pooled
  runtimes need it (D9).
- **The JIT's live heap outside the budget,** 384 MB against the
  interpreter's 110 after Octane, is found and bounded (stage 0).

Past the budget, as now, `makeRoom` drops lowerings, refusals, then code
unused for two epochs, least recently used first, and a compile with no
room is deferred; with the baseline beneath it, a refused optimizing
compile leaves the function in baseline code.

### The JIT built in and off: parity held by test

Stage 0's (d) built these; what each stage adds keeps them.

1. **The slow paths' hooks tested before the call.** Each is an inline
   test of the runtime's `jitOn` and a call behind it, never the JIT's
   body inlined: the additions' marks (`addMark`, `stringMark`,
   `addImmMark`, `addPropMark`, which no longer test the JIT themselves),
   the BigInt operators' `regBigOf` (a method, no longer a closure called
   on every BigInt result), `regSetProp`'s `jitNotePrimWrite`,
   `regColdWrap`'s `regColdWrapJIT`, a back edge's `regBackEdgeJIT`, a
   first call's `newRegCallsCounted`, a resumption's count, the native
   frame stack's `nativeGrowUncount`. A note that is the JIT's alone moves
   into the feedback dispatcher's handlers with the operand lattice
   (stage 1).
2. **What only the JIT calls lives in its files,** or where a range ends,
   so that it moves none of the interpreter's code: `bigRelease`, which
   had been 1,376 bytes among the BigInt operators, the marking forms,
   `regGenCaught`, `runRegFor`, `makeClosure`, `typedWrite`, the host
   hooks' init (`jit_hosthooks.go`), and the feedback dispatcher's
   handlers, after plain's (`native_handlers_feedback_<arch>.s`), not
   between them and the stubs every exit runs. `frameAt` and `topFrame`
   inline with the JIT built in as without it.
3. **`closureCode`'s hot fields first.** `jitCodeFields` is second to
   last, so that `rhCallW`, `regCallSlow`, `regRunFD` and `nativeLoop`
   read the call path's fields at plain's offsets (`reg` had been at 0xd0,
   not 0x70); `TestJITFieldLayout` holds it.
4. **Plain's layout-lock targets for the JIT's configurations,** as the
   WebAssembly configurations have (`TestJITConfigsAtPlainTargets`), with
   positions where the JIT's code before them had moved the interpreter's:
   KO after the JIT's files, IN after `New`, CC at `concat`, BU and CN in
   the native dispatcher's Go side, WE and LR in the call path.
5. **The gate.** `hotdiff -parity` between an untagged and a tagged build
   compares the hot set's instructions (but for the JIT's entry from a
   call), its and the parity rows' Go functions' offsets modulo 4 KB, and
   the slow functions that carry hooks within 96 instructions;
   `placements -micro`, plain against tagged with the JIT off, runs at
   each stage's end and each release, no micro-benchmark 3% or more slower
   at both placements.

What the lock holds no position for still differs: the closures after the
JIT's files' closures, the groups of generics the JIT's code instantiates
first, and the Go table's handlers after `rsAdd`, each hook in them a
test and a call longer.

### Inlining, loops and redundancy

- **Polymorphic inlining,** up to four targets a site, dispatched by a
  target check, as TurboFan takes four and Maglev inlines polymorphic
  calls. Today a site inlines only the one function its cache holds and
  its code learned it calls (internal/jit/README.md, "Inlining").
- **Sizes** calibrated to V8's bytecode (performance-plan.md, "Inlining
  sizes calibrated to V8's bytecode"), with TurboFan's bounds, 460 a
  callee and 920 cumulative, and the call frequency at 0.05: with one
  optimizing tier it takes TurboFan's bounds, not Maglev's tighter
  100-byte, 0.95-frequency ones.
- **The call sequence:** the frame-room check moves to the entry, one
  check for the largest callee window, `frameroom` being 3.6 to 4.0% of
  native samples on Richards and DeltaBlue.
- **Loops (W2):** OSR compiled apart, then the innermost loops peeled once
  under Maglev's 400-byte bound, with a loop-effects summary (the slots
  and shapes it writes, whether it calls) that lets the peeled
  iteration's checks and loads leave the body -- array checks, invariant
  objects' shape guards, upvalue loads, DeltaBlue's `this.v.elms` and its
  length -- and the WebAssembly optimizer's `licm` for the pure values
  left.
- **Redundancy across the CFG (W3):** `knownShapes`' facts meet at merges
  by intersection and survive loop headers for objects the loop's effects
  do not touch; the poll is shape-safe, since it runs no script; a
  transition forgets only its own object, and a fresh allocation is
  distinct from every other; stable shapes survive calls through W1's
  dependencies; the same facts cover `objectof`, kind bytes and `typeis`;
  field loads are eliminated and stores forwarded through a cache of
  (object, slot) to value that a maybe-aliasing store or a call kills;
  `keep`, `kept` and `propwrite`'s alias checks are trimmed where
  liveness allows. It is one forward walk of the dominator tree, linear
  in the function as `TestJITCompileLinear` requires.

**Why.** V8 measures Maglev's inlining at 1.37x the suite, 2.9x on
DeltaBlue, 2.4x on RayTrace and 1.7x on Richards, but most of it is what
comes after inlining. The calls V8 inlines and we call are 19 to 21% of
DeltaBlue's native time, 6 to 7% of Richards' and 1 to 4% of RayTrace's
and EarleyBoyer's; polymorphic inlining alone is +2.5 to 5% of the
composite. DeltaBlue's plan loop re-walks `this.size()` and
`this.constraintAt(i)`, about 230 instructions an iteration, because
`c.execute()` is an opaque call; they are hoisted only once `c.execute`
is inlined *and* `this` is not an OSR phi. With W2 and W3 the whole is
+5 / +8 / +11%. Trial inlining, which the baseline makes possible, is the
next project's (D12).

**Compile cost.** Inlining drives the build and emit phases; raising
HotSpot C1's inline size from 35 to 50 bytes cost it 24% compile time for
nothing. Peeling adds 3 to 8% on loop-heavy compiles (zlib, Mandreel) and
about 1% on Typescript, and OSR compiles are extra compiles; W3 is net
plus or minus 2% (+2 to 4% in build and optimize, -1 to 3% after). This
part makes its own case per phase on Typescript, Mandreel, zlib, PdfJS and
Gameboy, with W3's and W1's savings beside the inlining's cost, within
stage 2's gain (D7).

### Regular expressions and strings

The matcher compiles to machine code already (internal/jit/README.md,
"Regular expressions"); what is left in Go is the glue and the results,
which move to routines: the leaf's `exec` glue (`lastIndex`, the
captures' substrings); `replace`'s template expansion and its result's
building (`jitReplaceLeaf`); short concatenation, Splay's `joinParts`
through `jitConcatStub`, made natively into pooled noscan bytes; and for
longer strings the string parts V8's cons strings are, which
`partsBuilder` already is in the interpreter. Patterns the code refuses
(lookarounds, leftward reads, backreferences under `i` over UTF-16) stay
the matcher's.

RegExp is the suite closest to Node (2.2x); 66% of it is the leaves,
`regexpLeaf.match` 20% and `jitReplaceLeaf` 25%, and Splay's
concatenation is 20%. Halving the glue would give RegExp about 1.2 to
1.4x and Splay 1.1 to 1.2x (*estimate*), +3 to 7% of the composite, and
these are the gaps that widen on the M5 Max (RegExp's by 14%, Splay's by
10%).

### The backend

- **Branches and constants (W5).** A comparison feeding a branch is fused
  with it, never materialized; boolean phis (`||`, `&&`) are threaded
  into the branch that tests them, by the WebAssembly optimizer's
  `thread`; a `truth` of a known boolean or number folds; constants are
  immediates or stay in loop registers; two-address copies go. Richards'
  `(this.state & STATE_HELD) != 0 || ...` takes 63 instructions today --
  a materialized `ucomisd`, `setz`, `setnp`, a phi of boxed booleans and a
  `truth` -- against V8's 6, 37.7% of `schedule`'s samples.
- **Registers (W6).** DX (`Stack`) and a scratch register are freed, for
  nine allocatable general registers on amd64 against seven now; cell
  addresses are rematerialized rather than spilled to the context; the
  int32 phi budgets (`intPhiBudgets`, five now) follow the registers
  left. Stage 7's int32 work needs it on amd64: NavierStokes' inner loop
  has five int32 candidates besides bases and lengths. arm64 has the
  registers already.
- **Cell-free property access (W7).** Both words are read through the
  slot's address, `[table+off]`, after c1 `[obj+off]`, and the cell's
  address is never materialized, kept live or spilled; Richards spills
  three such addresses an iteration.

Together +1.6 / +3.3 / +6.8% of the composite, compile-neutral or cheaper,
changes in ssa and mir alone that touch nothing the interpreter runs.

### Compile speed

Typescript's compiles today, 643 at 0.69 ms each (`-jitcompile`, on the
Ryzen 9 9950X3D2):

| Phase | Share | What drives it |
|---|---:|---|
| build | 37% (164 ms) | the feedback's answers, 15% of a compile; the callees inlined, lowered and built |
| optimize | 18% (79 ms) | its rounds, which make the backend's input small |
| allocation | 20% (88 ms) | mir's linear scan: prepare, uses, liveness, intervals, hints, scan, saves |
| emit | 20% (89 ms) | instruction selection, edges, exits, 65 kinds of cold code (about 23% of the code) |
| encode | 5% (24 ms) | of it loop alignment 3 to 10% |

Outside the phases are the lowering, cached per function, the snapshot
and the placing, with two protection changes a compile; beside them, 77 MB
of garbage a run, and half of all compile time spent recompiling after
deopts.

What each part does to these phases is its stage's compile-time line
below. In sum (*estimate*): recompiles after deopts, half of today's
total, fall from 25% of compiles to 10% or fewer; the snapshot as a copy
of the caches' data, made once a function and reused by the callers that
inline it, takes the build 15 to 25% down; the WebAssembly optimizer's
workspace discipline (chunks, arenas, stamped tables) takes the garbage
from 77 MB a run to 20 and the time 5 to 15%; cold code as jumps to shared
routines takes emit 10 to 20%; `internal/jit/lsra`, the WebAssembly
optimizer's allocator, which takes 58 of its 168 µs a function, may take
allocation 20 to 40% down if it carries over, decided on `-jitcompile`.
Polymorphic inlining (+5 to 15%) and elements kinds (+2 to 5%) are what
must make their case; the wave as a whole moves a Typescript compile -1 to
+4%, every phase within +10%.

Of the WebAssembly optimizer, the backend is shared already (the
assemblers, the arena). Its allocator, its workspace discipline and three
passes carry over -- `bce`, bounds from values, once indices are int32s;
`thread`; GVN's stamped tables -- and `licm` serves W2. Its IR does not:
JavaScript needs guards, frame states and exits, which typed code has no
use for.

### Both architectures

Every part lands on amd64 and arm64 together, as the backends always
have. nativegen emits both architectures' stencils and routines; arm64's
stencils patch imm12, imm19 and imm26 fields and carry large-offset
variants; jumps from the arena into Go's text use `BR Xn`, since arm64
has no 128 MB guarantee; each placed batch flushes the instruction cache
once. Each stage's arm64 code runs under qemu's linux/arm64, and the
integration in CI's `ubuntu-24.04-arm`. Timing stays on the Ryzen 5 3600,
and the maintainer checks each stage's per-suite ratios on the M5 Max
(D11), where the Go-bound suites (DeltaBlue, Splay, RegExp) are wider, so
stages 7 to 9 weigh more there. Hardened darwin processes stay
interpreted; a path through libSystem's JIT write-protect toggle without
cgo is to be verified, not assumed.

### Open items

What the work found that the architecture above does not yet place.

- **Native await.** Done. Stage 1's operand lattice found it:
  `async_await` +4.9% with the JIT at every placement, the feedback
  handlers' recording in a loop that stayed interpreted because each
  `await` left native code. An async function's await that the
  interpreter goes on past in place -- the frame resumed by its await's
  job, no job queued, the value settled -- native code goes on past in
  place too, with no exit (ssa's `OpAwaitReady`, `GoAwait`;
  internal/jit/README.md, "What is entered where"), and an async
  function's call is a settled call of Go to the profit model. A
  suspension that waits still returns to Go's job queue, whose promise
  side stays Go's. Measured with the JIT at `-k 2`: `async_await` 20.35
  to 4.70 ns (-76.9%), `async_busy` 5.94 to 4.33 (-27.1%), the other
  async and generator rows level, the micro set's geomean 0.998, Octane
  +0.2% and ssr +0.1% (none awaits), compiles unchanged. Projected:
  `async_await` -71 to -83%, `async_busy` -16 to -18%, `async_call` -7
  to -13%, which measured level, its loop judged slow for its calls of
  the async callee. Suspensions made by Go at once, and for await's and
  yield*'s steps as calls of Go, measured within 1.3% and were left out.

## The stages

Each stage is a round of its own: one integration, one CI run, its own
commits, and internal/jit/README.md updated in the commit that changes
the design it describes. Durations and sizes are *estimates* for one
agent's work with its reviews. Every stage's gate includes:

- **The interpreter no slower:** `placements build -k 2` plain at the base
  against plain at the change, `compare -micro` and the V8 and Octane
  suites with the JIT off; no micro-benchmark 3% or more slower at both
  placements, and the suites level within placement noise.
- **The JIT built in and off no slower:** the tagged build with the JIT
  off against plain, by the parity gate above.
- `hotdiff` or the layout lock regenerated, `TestIntendedInlining`, and
  the compile gates: `-jitcompile 10` per phase on Typescript, Mandreel,
  zlib, PdfJS and Gameboy, and `TestJITCompileWork` within its bounds or
  rewritten with the reason in the commit.

The projected composites are the Ryzen 5 3600's, cumulative; the model
is under [The projection](#the-projection).

### Stage 0: correctness and hygiene

1 to 2 weeks, about 2,000 lines, low risk.

| | What | Gate |
|---|---|---|
| a | Babel's lost method receiver: `this` undefined in `parseExpressionBase` on its second transform of the V8 suite's crypto.js | a regression test with the exact error; the bundle corpus with the JIT |
| b | ssr's background exit storm: 348,344 `get_prop` host exits at `-n 10`, not at `-n 3`; some site compiled from a snapshot leaves at every call | ssr's Render in the background under `QJS_JIT_STRESS` `background=K` with its exits bounded; the snapshot's answers checked against a spot compile's |
| c | `-jitreport`'s host-exit counters going negative across benchmarks in the background | a report test over two benchmarks in the background |
| d | the parity items 1 to 3 and their gate | micro-benchmarks plain against tagged and off: string appends, BigInts and generators within 2% |
| e | the JIT's live heap, 384 MB against the interpreter's 110 after Octane | the live heap after Octane at most the interpreter's plus 64 MiB; Typescript's collector CPU measured before and after |
| f | the budget: 64 MiB by default, `WithJITMemoryLimit`'s documentation (its 3,750 ms at 8 MiB is out of date), the flush when control returns to the host | the memory table again at the default; the RSS of 100 idle runtimes |
| g | `jitBudgetForFeedback`'s comment: it cites a V8 flag 14.6 no longer has; it says what the constant does here, when a loop's back-edge budget starts, and keeps 940 (its value moves with the baseline tier, stage 5) | the comment; no change of behaviour |

- **Gain:** the composite 0. Typescript nothing at 32 MiB, but at the
  default it stops losing 1% (one run) to 34% (three runs). Goals 4 and
  5 met.
- **Compile time:** none per phase; less garbage if (e) is in what the
  compiles keep.
- **Takes up:** performance-plan.md's "The default room for code" (f)
  and "The JIT's Go side past JN" (d).
- **Status:** done but (d)'s residue, which is open. (a) is fixed by
  `0905382a`, Go leaving a native level's receiver as native code left
  it, with its regression test, its pads in `379a60ad`. (b) is fixed by
  the install of a compile in the background leaving its function marked
  to be compiled again for what the code it replaces learned meanwhile
  (`jitRegJob`'s `asks`): the snapshot was right -- a build from it the
  same as one on the spot at each of more than 2,100 compiles in the
  background of ssr, Are We Fast Yet and Octane -- but React's
  createElement met "div" at `a.defaultProps` while it was compiled, and
  the install cleared the mark. ssr's Render at `-n 10` in the
  background, five runs: 41,238 to 43,770 exits to Go, 662 to 2,700
  finished in the interpreter, at most 88 `get_prop` host exits (two runs
  of three had made 437,000 to 450,000, 357,000 and 348,000; the
  foreground makes 41,139, 686 and none). Under `QJS_JIT_STRESS`
  `background=K` at `-n 5`, K = 1, 32 and 512 had made 172,431 `get_prop`
  host exits; now none at K from 1 to 512. On a Ryzen 5 3600 Render in
  the background runs 1,670 to 1,738 ms, five runs, where the storm had
  made it 2,213 to 2,246 in four of five; on the spot 1,704 to 1,742.
  (c) is fixed in the report and in what it reads: a benchmark in a
  runtime of its own, as ssr's and fresh's are, is counted from nothing,
  not from the last one's runtime; functions are told apart by their
  column, a line's "(anonymous)" ones each measured against another's
  counts before; and a function's native callers' calls are counted over
  its compiles, its link's count begun afresh at each (`jitEntry`'s
  `calledIn`). No count is negative over Octane, the V8 suite, Are We
  Fast Yet, ssr and fresh at `-n 3`, on the spot and in the background
  (5 in Octane and 18 in fresh before). (d) is built (the parity items,
  "The JIT built in and off: parity held by test"), its gate `hotdiff
  -parity` passing; plain against tagged and off, `placements -micro -k
  2` on the Ryzen 5 3600: the geometric mean 1.003 (1.007 before), 18
  tests 3% or more slower at both placements (35 before). String appends
  are all within 2% (7 to 16% before); BigInts within 2.2% but
  `bigint64_divmod` +4.6% and `bigint1024_arith` +7.1% (`xorshift`
  +1.9%, `fnv1a` +2.2%, from +16% and +10%); `forward_call2` and `6` -4
  to -5% (+11%). The plain build against the base: 0.999, `asm_table`
  +7.8% and `object_create_fresh` +3.5% at both placements. (e): what
  kept the heap was the runtime's asm.js heaps (zlib's 128 MB buffer),
  the idle compile workspaces (32 MB) and the code cache, whose code and
  feedback held the objects their code was compiled against; asm.js
  heaps are held weakly, the workspaces no compile used in a turn go as
  the runtime returns to its host, and code is parked then, once the
  program's running since the last parking has paid 200 times for it,
  its objects held weakly and checked alive at its next use
  (internal/jit/README.md, "Parking"). The live heap after Octane fixed
  -n 1 with the JIT is 149 to 151 MB in every run, three on Windows and
  three on Linux (Ryzen 9 9950X3D2), against the interpreter's 107 to
  108, from 357 to 390; projected, the interpreter's plus 64 MiB. Parked
  once a flush age of the program's running instead, it had been 156 to
  161 MB only in the runs whose one parking fell after Typescript, every
  run on Windows but four in fifteen on Linux, and 350 MB in the others.
  At the default budget (Ryzen 5 3600, -n 3, one runtime): Octane
  10,618 to 8,681 ms, live 422 to 152 MB, peak RSS 839 to 618 MB; Typescript 5,438 to 4,183 ms, live 288 to 148 MB, its
  collector's CPU 1,469 to 1,138 ms; 30 idle runtimes that ran the V8
  suite 697 MB live and a 922 MB working set, from 792 and 999. Active
  code's speed, -k 2 with the JIT against 806bfca5, parking paced by its
  cost: micro-benchmarks' geometric mean 0.998, Octane +0.0%, ssr +0.7%,
  fresh +1.3%; against parking once a flush age, fresh +0.1%, ssr +1.1%. (f): the
  default is 64 MiB, `WithJITMemoryLimit`'s documentation and the docs
  that cited 8 MiB say so with today's figures, and the cache looks to
  flush as the runtime returns to its host; its memory table and the RSS
  of idle runtimes are measured with (e). (g): `jitBudgetForFeedback`'s
  comment says what the constant does and when a loop's back-edge budget
  starts, 940 kept.
- **Open:** (d)'s residue, tagged and off against plain at both
  placements: generators +5 to +10% (`generator_objects` +10.4%,
  `generator_create` +6.1%, `generator_for_of` +5.4%), `dataview_read`
  +16.6%, `array_reduce_upvalue` +14.7% and `has_own_method_hit` +9.6%,
  whose Go code is at plain's offsets. What moves them is what the layout
  lock holds no position for: the closures after the JIT's files'
  closures, the groups of generics the JIT's code instantiates first, and
  the read-only data. With the JIT on, the three changes together against
  806bfca5 had 10 micro-benchmarks 3% or more slower at both of
  `placements -micro -jit -k 2`'s placements; at `-k 4`, base against
  change at each placement in ns, none is 3% or more slower at all four:
  `map_set_bigint` 111.41/117.34, 110.10/113.80, 112.19/117.12,
  112.42/114.88; `string_trim` 54.83/61.30, 56.81/64.95, 67.96/59.72,
  55.41/70.72; `array_every_upvalue` 7.75/8.20, 8.03/8.04, 7.82/8.21,
  7.78/8.26; `regexp_property` 262.05/274.69, 268.68/276.43,
  256.98/253.56, 250.21/263.25; `array_some` 9.24/9.71, 9.58/9.67,
  9.01/9.65, 9.49/9.61; `map_set_int` 111.31/118.01, 110.45/117.55,
  115.31/114.50, 120.35/114.56; `array_indexOf` 90.47/106.96,
  89.50/99.09, 93.52/90.69, 111.10/90.36; `dataview_write` 52.48/57.20,
  52.06/52.77, 53.30/53.28, 52.71/50.48; `json_stringify_strings`
  30.27/30.67, 31.61/31.99, 30.78/31.28, 30.47/30.90; `asm_adler`
  1.81/1.76, 1.66/1.66, 1.84/1.80, 1.86/1.82. All but `map_set_bigint`
  are level at some placement: placement. `map_set_bigint`, more than 2%
  slower at all four (+2.2 to +5.3%), is layout: the JIT does the same
  work in both builds (`-jitreport`'s exits and the allocations the
  same), and the Go code it runs is the same code moved to plain's
  offsets modulo 4 KB, as goal 4 asks of the JIT's configurations --
  `jsMap`'s `set`, `find`, `add`, `lookup` and `rebuild` each 0x780 on
  (`set` 0x880 to 0x100, `find` 0xe60 to 0x6e0), where the plain build
  has them; `trimString` 0x5c0 to 0xea0 (plain's 0xe80) and
  `arrayIndexOf` 0x540 to 0xd40 (plain's) the same.

### Stage 1: tiering and background compiles

4 to 6 weeks, 4,000 to 6,000 lines, medium risk. Depends on stage 0.

- **Builds:** the operand-type lattice in the feedback variant on both
  architectures and the builder speculating on it; the deopt discipline's
  first half, the cap removed and the per-site marks ending deopt loops
  (the delay bit, the back-off and the raise wait for the baseline tier,
  stage 5); the background default with the one-P
  policy, the most-called-first queue with its stale drop, placement
  apart and the take-back.
- **Gain:** the composite +0 to 2% (EarleyBoyer's 29 deopts and Earley's
  86 runs finished in the interpreter fall); Typescript in the background
  1,713 to 1,600-1,680 ms, and at GOMAXPROCS 1 back to the synchronous
  2,573 ms from 3,278. It is the prerequisite of stage 7's int32 work.
- **Compile time:** recompiles after deopts -10 to 25% (an estimate:
  the lattice's right first compiles and the per-site marks); the build +1
  to 3% for the lattice's questions.
- **Gate:** Typescript's compiles, deopts and recompiles from the tier
  trace; the warm-up at GOMAXPROCS 1 back to synchronous; Are We Fast Yet
  and ssr no slower in the background (today +2% and Render +14%);
  differential fuzzing with background installs.
- **Status:** done but the residue under Open. The operand lattice
  is built (internal/jit/README.md, "Feedback"): the feedback dispatcher
  ORs each arithmetic, comparison and bitwise site's operand kinds into
  its byte on both architectures, and the builder speculates on it -- a
  string or an object generic and Go's from the first compile, a site
  that met nothing left to Go at a host exit that invalidates nothing,
  BigInts by their marks, which know the results; a guard's failure at
  such a site widens its byte before the interpreter runs it. Oddballs
  beside numbers are still learned at a guard: built generic, the sums
  of native loops' booleans had been Go's, and the loops with them. A
  site that met nothing counts only once the interpreter has run its
  function to a return 8 times before any compile, V8's feedback point.
  A handler tests the byte before it stores, as V8's feedback update
  does (a stable site's compare-and-branch 30 instructions, not 12, no
  store). Measured with the JIT against 9d14b87b: Octane -1.0%
  (Mandreel -1.8%, EarleyBoyer -1.3%, PdfJS -1.2%, Typescript -1.0%;
  Ryzen 5 3600, -k 2); the micro-benchmarks' geometric mean +0.2%.
  Deopts (the tier trace): Octane 344 to 338, its compiles after deopts
  263 to 256 (projected -10 to 25%), PdfJS 37 to 31 and its compiles 142
  to 131; Are We Fast Yet 78 to 74; the V8 suite 9 and EarleyBoyer 4,
  unchanged; Typescript 193 to 194, its deopts property sites', which
  the per-site marks and stage 5 are for. The build (`-jitcompile 10`):
  -3.3 to -4.7% on Typescript, Mandreel, PdfJS and Gameboy, zlib level
  (projected +1 to 3%), no phase 10% slower.
  The deoptimization discipline's first half: `jitMaxDeopts` removed, no
  function refused for its deoptimizations; each site's deoptimizations
  counted, and at the eighth (`jitSiteDeopts`), past every site's
  ladder, the site built at its top, so that a function's are at most
  eight times its sites (`TestJITDeoptsBoundedBySites`: 96
  deoptimizations of one function at 24 sites, which the cap had refused
  at 65). Measured, -n 5 with the JIT on the Ryzen 9 9950X3D2: no
  function of the V8 suite, Octane, Are We Fast Yet or ssr was near the
  cap -- the most 14 of one function, Typescript's, 3 at one site -- and
  none reaches the bound after; Typescript's compiles 768 (201 after a
  deoptimization) and its deoptimizations 214, against 768 (202) and 215
  before: as projected, the cap's removal costs nothing, and the
  recompiles' fall is the lattice's.
  The background default: `WithJIT` compiles in the background where
  GOMAXPROCS is more than 1 and on the spot where it is 1, decided at
  each compile, `WithJITBackground(false)` the opt-out and `true` the
  background at any GOMAXPROCS; `qjs --jit` takes the default,
  `--no-jit-background` the spot; v8bench, placements and the
  conformance runner keep the spot unless `-jitbackground`. A function
  waiting for its compile counts a budget between its asks, where each
  of its back edges had been a call of Go; the queue serves the job
  whose function did most work waiting, and a job unbegun whose function
  did none from one flush tick to the next is dropped; a job no worker
  began by its function's second ask is taken back (the *estimate* kept
  at two asks, each a budget); the workers place their code apart
  (`NewSSACodeApart`), the install a store. `QJS_JIT_STRESS`'s
  `takeback` gives no compile to a worker, each taken back, which the
  differential tests run among their settings. Ryzen 5 3600, medians of
  five fresh processes: Typescript in the background 1,715 to 1,642 ms
  (projected 1,600 to 1,680), on the spot 1,927 and 1,932; at GOMAXPROCS
  1 the default is the spot's 2,459 ms (2,475 at the base) where the
  background took 3,239 (projected back to 2,573 from 3,278), and the
  background forced there takes 2,914 with the take-back. The V8 suite's
  five runs at GOMAXPROCS 1: 1,330 ms in the background at the base, 935
  by default now (899 on the spot at the base), 1,072 in the background
  forced. `placements -k 2 -jit`, in the background, base to change: Are
  We Fast Yet 4,954 to 4,914 ms, on the spot 4,917 to 4,887 (the
  background +0.6%, from +0.8% at the base and +2% when this plan was
  made; DeltaBlue +2.7%, Sieve +2.0% the most); ssr's Render 170.4 to
  170.5 ms against 174.4 on the spot, FirstRender 48.7 against 54.9.
  The three together, with the JIT against 984acb8a (Ryzen 5 3600):
  Octane -0.3% at -k 4 (Mandreel -1.9%, Typescript -0.1%; -0.0% at -k 2,
  and -0.7% for the lattice alone there, against the -1.0% it measured
  on 9d14b87b: within the runs' spread); the micro-benchmarks' geometric
  mean 1.002 at -k 2, and of its seven tests 3% or more slower at both
  placements none 3% or more slower at all four of -k 4's;
  `typed_i64_map` is slower at all four (+2.1 to +9.2%), but its JIT
  work is the same in both builds (the report's compiles, exits and
  calls of Go) and it is level, +0.6%, in the lock's own builds:
  placement. The interpreter: plain against plain 1.000 at -k 2; the JIT
  built in and off against plain 1.003, 1.004 at the base. The build
  (`-jitcompile 10`, two runs each, a run's total): Typescript -1.0%,
  Mandreel -3.2%, zlib -3.6%, PdfJS -3.8%, Gameboy -3.3%; no phase 10%
  slower a compile -- Mandreel's build is +10.7% a run over 84 compiles
  where the base makes 78, the lattice's compiles for what its host
  exits taught (11 against 4), +2.8% a compile.
- **Closed since:** `async_await` +4.9% with the JIT at all four of
  `-k 4`'s placements (+5.7% integrated, slower at three): the lattice's
  recording, the feedback handlers' test of each site's byte, in a loop
  that stayed interpreted because each await left native code. Native
  await (the architecture's open items) runs that loop natively: 20.35 to
  4.70 ns.

### Stage 2: compile speed

3 to 5 weeks, 3,000 to 5,000 lines, low risk. Depends on stage 1.

- **Builds:** the snapshot as a copy of the caches' data; lowering on the
  worker; the WebAssembly optimizer's workspace discipline in ssa and
  mir; lsra tried for mir, landing only if it wins per phase.
- **Gain:** Typescript in the background about 1,640 to 1,470-1,600 ms,
  mostly the runtime goroutine's 11% shrinking, which may meet goal 2 at
  its middle; the composite 0.
- **Compile time:** a compile 0.74 ms to 0.50-0.60; garbage 77 MB to 20
  to 40; the goroutine's share 0.3 ms to 30 to 60 µs.
- **Gate:** `-jitcompile 10` per phase on the five Octane programs, no
  phase slower; `TestJITCompileWork` rewritten downwards; `compilediff
  native` the same code for every compile, since the stage changes how
  code is made, not what.
- **Status:** under way. The goroutine's part of a compile in the
  background is measured now (`JITCompileClock`, `-jittier`'s line):
  on Octane's Typescript, 620 compiles, 256 µs each prepared and 76
  installed at the base (Ryzen 5 3600, medians of three to four runs),
  of the preparation about 27 µs the lowering, 195 the snapshot (145
  the function's questions, 25 of them its callees' lowerings, 48 its
  callees'), 5 the hand-over. Two steps so far: the workspace taken
  and given back on the workers, its rewind off the goroutine, an
  install 76 to 40 µs; and the snapshot's answers kept as their
  methods' results in tables by argument, their effects as spans of one
  list, no map, no boxing, no table grown an answer at a time, the
  snapshot's allocation 172 to 79 KB and the preparation 256 to 214 µs.
  So 332 µs a compile to 254, projected 30 to 60; Typescript in the
  background 1,643 to 1,598 ms at `-k 2` (-2.7%), projected 1,470 to
  1,600 for the stage. What is left is the feedback's own work, which
  runs on the goroutine because it reads the live heap and writes the
  runtime's state: of a snapshot's 111 µs, made again warm, `Inline`
  43% (its callees' questions 27), the property sites' 23%,
  `NativeCalls` 15%, the rest small methods and allocation. The
  snapshot as a copy of the caches' data, the methods split into what
  they read, copied ahead, and what they derive, on the worker, their
  writes deferred to the install, is the next step, method by method in
  that order, then the lowering on the worker (*estimate*: 4 to 6
  weeks, 3,000 to 5,000 lines). What each must copy rather than read
  on the worker: the caches, which the interpreter writes; what the
  code learned, which the goroutine writes at exits; the fields of the
  shapes the answers name -- a unique shape is changed in place until a
  cache or a compile remembers it, and a shape's index is built when
  first needed -- and what the methods write besides their lists
  (`remember`, the learned lists, the literals' and known reads' caches,
  atoms, a callee's register code, cells and pools), deferred to the
  install or made ahead. The methods that answer from what the code
  learned and the interpreter's marks are derived on the worker: 16 of
  them, about 240,000 of the 597,000 questions a snapshot asks over
  Octane's functions (`Generic` alone, asked at every PC, 118,000), and
  about an eighth of a snapshot's time in a profile of it made twenty
  times over (projected 15 to 25 µs of the goroutine's part); the
  goroutine copies their lists' headers and the interpreter's marks once
  a snapshot. *Measured*: with the stage's next steps. So are the seven
  methods that compare a call's cached callee with the realm's built-ins
  (`CharCode`, `Substring`, `IndexOf` and the like, 71,000 questions, a
  few hundred answered), from the closures' call caches copied whole
  once a compile, which `NativeCalls` and `Inline` read next.
  `NativeCalls` is derived on the worker from those copies, with
  `Resumed`, `EntryGeneric` and `ViaLeaf`, which read what it learns:
  about 49,000 questions over Octane's functions, 18% of the
  snapshot's time in the profile made twenty times over (projected 15
  to 30 µs of the goroutine's part). The goroutine asks it only where
  it reads the heap or makes what the runtime keeps -- a construction
  it learned no pool at, `super()`, `apply`, typed code, a built-in's
  checks -- and takes the
  facts of the targets the copies name and their links, and of each
  call site, once a closure, in one pass over its code (a binding's
  function now, a method's seen callees). Over Typescript's 600
  compiles in the background, 45,749 of the PCs NativeCalls is asked
  at are derived; of the 3,700 asked on the goroutine, 2,723 were
  calls the interpreter had not made, now read in the pass, and 765
  constructions, now derived where the code has learned their pools. What the answer learns, the appends to the learned
  calls, the finish writes once the compile is done with, as V8's
  concurrent compiles write no feedback. *Measured*: with the gate's
  `compilediff` and the goroutine's part, to come.
  The goroutine's times leave out the host's hooks (the tier trace's
  line and its flush, `-jitcode`'s record), which had been part of each
  install. Lowering ahead, on a worker: a compile in the background
  whose function's program is not lowered for its hints hands the
  lowering, and its callees' that the call sites' caches name, to the
  workers and is asked for again a quarter of a budget later, taking the
  workers' programs, the same ones, at its next ask
  (`jit_lowerahead.go`); projected 50 µs a compile off the goroutine (27
  the root's, 25 the callees', of which a scan of the caches finds about
  80%). One lowering a function: an entry's, a hot function's and a
  callee's programs differ only in what they refuse, so a function is
  lowered once for the three (`LowerRegKindsWith`) and the kinds share
  the program, its bytes counted once; of the lowerings Typescript made
  in the background, about half had been a second kind of a function
  lowered before (86 of the compiles' own, 154 of the callees'). The
  feedback's lists put back on the worker (`replay`), not at the
  install; projected 12 µs of the install. *Measured* (Ryzen 5 3600,
  Typescript in the background, hooks left out on both sides, µs a
  compile, median and mean of 620): the four on the snapshot's
  scaffolding alone, preparation 101 and 207 to 77 and 174, with about
  5 µs a compile of asks waiting for a lowering ahead beside it; install
  13.1 and 29 to 10.8 and 16; Typescript 1,618 to 1,579 ms. With the
  property sites', the learned methods' and the built-in calls'
  derivations (the stack, three runs each): preparation 101 and 207 to
  60 and 130, install 13.1 and 29 to 10.5 and 15.8, Typescript 1,618 to
  1,561 ms (-3.5%), ssr's FirstRender 235.5 to 230.3 ms (-2.2%). The
  goal is preparation 15 and 30, install 10 median. Then the install:
  the workspace given back unrewound, rewound by the worker that takes
  it next (3.6 µs of the install to 0.4); the code's description made
  on its worker (the table of the register code's length, its loops'
  and returns' scans); a keep's links made each once on the worker,
  their hold counting each with no search (keeps of 25 links on
  average, 626 at most, two thirds of them repeats); each function's
  hints taken once a compile. The install, by phase timers on the
  stack: 14.5 µs a compile to 9.0-9.5, against the base's 30.8 (the
  `-jittier` line's mean). What is left is the cache's bookkeeping,
  which reads and writes the runtime's tables (the code it replaces let
  go of, its link, its callers joined), about 4.7 µs, of which the
  measures of a function's learned state (`learnedBytes`, a walk of its
  lists at each account) are the most; kept current as the lists grow,
  rather than walked, they would be a later option. The hand-over of a
  lowering ahead costs 10 µs, 4.0 its callees' scan and 3.5 the
  workers' queue and a worker started; once the snapshot's gathers need
  no program, a compile hands over once and its worker lowers the root
  and its callees as it derives, keeping them at the install
  (`regKeepLoweredAt`, the hints copied by `jitRegHintsCopy`), and the
  lowering ahead goes. A worker that waits a moment for its next task,
  woken by the hand-over rather than started anew, was tried: the
  hand-over 4.0 to 3.7 µs, within the runs' noise -- waking the
  worker's thread is the cost, whether it waits or is started -- and
  dropped.
  The snapshot is keyed by register pc: its gathers read the register
  code and the copies, never the program, which is bound to it after
  (`bindProgram`, `jitSnapBinders`), so that a callee's state may be
  gathered before anything lowers it. The callees a compile may inline
  were walked before it asked anything (a walk since replaced, below): every
  closure the function's state names as one it calls or makes, to the
  snapshot's depth, of register code short enough to inline
  (`ssa.MayInlineSize`, one size bound with the inliner's); over
  Octane's functions it finds every callee the snapshot asks (1,226 of
  2,071 walked, so 1.7 times as many copied). A function never called
  has no register code, and names nothing until the asking makes it;
  the walk makes the register code of the targets its calls' bindings
  name, as the asking does, 14 functions over Typescript's run beyond
  the base's, 0.3 µs a compile. The constants, the captured typed
  arrays and `RawMath` are derived on the worker too. Where a
  snapshot's time on the goroutine goes now (QPC, exclusive, Typescript
  in the background, µs a snapshot at the mean, 3.8 callees each):
  `Inline` about 45, with its callees' lowerings on the goroutine 9,
  their call targets 10, their seen feedback 5; the gathers 44, the
  calls' two 28 of it and the property sites' 12; the no-argument
  questions 7.5; the rest of the asking (the program bound, the PCs'
  loop) 13; every other method still asked at the PCs under 10 in all.
  So the methods left are small, and `Inline` and the gathers are what
  remains, both of which go with the callees' snapshots gathered from
  the walk and the targets gathered of the register code. Tried and
  dropped: `Inline` asked at calls alone (192 host operations over the
  corpus were no calls); the copies taken of chunks a compile allocates
  (18 MB more garbage over Typescript than the 5.5 they saved, the run
  2 to 5% slower; sized to their first ask, 8.4 against 5.5); the
  no-argument questions asked once a compile, of which 6 of 13 are the
  compile's whole (about 2 µs, a later option).
  The calls' targets' share of the goroutine (`NativeCalls`' gather and
  the call sites' pass), about 5.6 µs a compile at the median and 15 at
  the mean, is first touches of the heap, not computing: warm, it is 0.8.
  Of it (QPC, Typescript in the background, µs a compile, timers' own
  cost in each figure) the targets' facts are 0.4 and 1.2, deciding
  their links 0.2 and 0.5, finding the links 0.7 and 1.6, the call
  sites' pass 2.3 and 7.1. Tried on these numbers and dropped: the
  facts' fixed parts (the function's flags, its register code's) read
  on the worker, the gather taking only what changes -- the gather must
  decide each target's link, which the code embeds before the install,
  and the decision reads those parts, so the most it saves is 0.3 and
  1.2; and the link decided on the worker and made at the install,
  which needs the code relocated there and moves the link's making onto
  the install rather than removing it. A link found by the hint slot
  its template names, before the map of links: the targets' gather
  4.1 and 10.2 to 3.6 and 9.1 (three runs each). The call sites' pass
  making its lists at once and keeping its table by register in the
  runtime: 2.5 and 5.9 to 1.7 and 4.6.
  The inline sites (`Inline`, `ForEach`, `CalledClosure`) are derived on
  the worker now, the callees they may take decided on the goroutine by
  `Inline`'s own decision before it lowers (`jitInlinePrefix`), asked by
  register pc of the gathers' copies, its calls' targets kept for the
  derivation; each callee taken a node gathered of its register code
  alone, lowered on the worker of its hints copied, kept at the install.
  The walk it replaced named 2.7 times the callees taken (Typescript:
  ~5,600 walked a run, ~2,040 taken), each gathered whole, and made the
  goroutine's part slower than the base's (prep 73 µs median against
  61); taken by the prefix, 97% of the callees gathered are taken
  (~2,340 and ~2,270), none missed over Octane's functions. *Measured*
  (Ryzen 5 3600, Typescript in the background, 4 runs each alternated,
  QPC): prep 60.3 µs median, 125 mean at the base to 60.1 and 116;
  install 5.7 and 14.9 to 6.6 and 16.9, the callees' entries and
  lowerings kept there; the run 1,552-1,598 ms to 1,549-1,569. The
  compile's own function may be gathered of no program too
  (`jitRegSnapshotOf` of none, `program` binding it on the worker,
  `CellsPool` derived there): over Octane's functions 733 of 738 are, the
  other 5 taking a forwarder. The root's lowering on the worker is next,
  with the lowering ahead retired.
- **lsra for mir:** tried, and it does not land. `internal/jit/lsra`
  was run beside mir's scan on every compile, given the same function: an
  instruction for each value and the block's end, the frame states' uses
  as mir's use walk finds them (each value's last in each block), the
  phis, the hints, and no clobbering calls, since mir's saves keep what
  lives across a call. Its result was thrown away, and its time was a
  phase of its own. The table gives ms per run of all compiles, measured
  with `-jitcompile 2` or `3` on the Ryzen 5 3600 while another job ran
  (rough). mir is its liveness, intervals, hints and scan, the phases
  lsra would replace; lsra is its own phases, and in brackets with
  building its function:

  | Program | mir | lsra |
  |---|---:|---:|
  | Typescript | 45.5 | 63.6 (76.5) |
  | zlib | 28.8 | 82.3 (91.7) |
  | Mandreel | 8.0 | 12.1 (14.8) |
  | PdfJS | 10.7 | 13.1 (15.5) |
  | Gameboy | 7.6 | 15.5 (19.2) |

  That is 1.45 to 3.2 times as long, before its locations are mapped
  back, with mir's uses and saves still needed. Projected: allocation
  20 to 40% down. lsra's liveness and scan are cheaper than mir's
  (Typescript 11.8 against 18.6 ms, 16.8 against 21.7). Its range lists
  and its webs' coalescing are not: each register gets a range a block
  it is live in, built from dense live-out sets, and each web is
  checked against another's lists. Together they take 28 ms where
  mir's single-span intervals and hints take 5.2. On zlib, its slot scan
  with holes alone takes 21.6 ms. Its liveness sets are dense, blocks
  times registers live across a boundary, which the WebAssembly
  optimizer bounds by refusing past `MaxLiveBytes`. The JIT refuses no
  function for its size, so mir's scan would have to stay behind it as
  a fallback. In code, weighing each spilled value by mir's cost in a
  slot, lsra without splitting spills about as much on Typescript
  (+0.8%), more on PdfJS (+19%), Mandreel (+32%) and zlib (+3%), and
  less on Gameboy (-28%).
- **Lead, for stage 7b:** in the same comparison, lsra's phi
  coalescing leaves 78 to 96% fewer weighted edge moves than mir's
  hints and groups: an edge whose phi and argument differ in location,
  weighed as the edge runs. That holds on Typescript (2.40M to 0.53M),
  Mandreel (10.6M to 0.37M), PdfJS (0.40M to 0.07M) and Gameboy (82K to
  5K), but not on zlib (0.64M to 2.24M), where mir's spill classes share
  the phis' slots. The lead is mir's own coalescing, as part of the
  backend's work, not lsra.
- **Workspace discipline in ssa and mir:** landed. Measured compiles
  1.2 to 3.2% faster, against the 5 to 15% projected, and garbage down by
  40 to 85% (`-jitcompile 10`, three alternated rounds, Ryzen 5 3600):

  | Program | ms per run | heap per run, MB |
  |---|---:|---:|
  | Typescript | 390.8 -> 384.9 | 60.3 -> 35.6 |
  | Mandreel | 82.2 -> 79.6 | 12.7 -> 7.6 |
  | zlib | 131.3 -> 127.2 | 19.7 -> 3.2 |
  | PdfJS | 78.8 -> 77.7 | 11.3 -> 8.2 |
  | Gameboy | 77.9 -> 76.9 | 11.4 -> 8.6 |

  No phase is slower. Most of the garbage was the workspaces making their
  tables again. A trim counted by jobs gave back a big compile's chunks as
  it ended, and the next compile as big made them again. The arenas and
  mir's scratch tables now keep what the compiles of the collector's
  current and last cycle needed, as `sync.Pool` does. A pooled workspace
  still goes whole when idle. Typescript's plain run allocates 611-659 ->
  512-529 MB, one collector cycle fewer; its wall time is level. What was
  saved was mostly bytes, which cost the collector more than the compile
  does; that is why the time gain is smaller than projected. Beside that:
  the builder's guards are a method rather than a closure, so their
  arguments stay off the heap, and a compile's lists are in its
  workspace. TestJITCompileWork measures each compile in warm workspaces
  too. What is left of Typescript's garbage is the code's own
  descriptions (about 12 MB of `-jitcompile 3`'s 34.5), the feedback's
  answers (about 8 MB and 64k objects, the snapshot's to remove), and
  mir's cold code as closures (about 50k objects, for stage 6's shared
  routines). `compilediff native` gives the same code for every compile
  (V8 suite, AWFY; Octane with `-any`, within a base-against-base
  control).

### Stage 3: closures and cells natively

2 to 4 weeks, 2,000 to 3,000 lines, low to medium risk. Depends on stage
0.

- **Builds:** closures, cells and arguments objects made natively, and
  the level in the context.
- **Gain:** Earley 1.7 to 2.1x, EarleyBoyer 1.3 to 1.45x, the composite
  +3.3 to 4.7%, and the JIT faster than the interpreter on Earley;
  Typescript about 0.
- **Compile time:** emit +1 to 2% for the pool takes.
- **Gate:** no `jitRegFrameOp` in Earley's profile; a test that the JIT
  is not slower than the interpreter on Earley; the marking rule's tests
  for the new stores (`jitMarkingForTest`, `TestJITSameWorkWhileMarking`).
- **Status:** not started.

### Stage 4: inlining, loops and redundancy

7 to 10 weeks, 6,000 to 9,000 lines, medium risk. Depends on stage 2's
compile headroom.

- **Builds:** polymorphic inlining with the calibrated sizes, the frame
  room checked once at entry; W2, OSR compiled apart, then peeling with a
  loop-effects summary and `licm`; W3, facts across the CFG, load
  elimination and store forwarding.
- **Gain:** the composite +5 / +8 / +11%; at the middle DeltaBlue 1.33x,
  EarleyBoyer 1.44x with stage 3, Richards 1.12x, NavierStokes and Crypto
  1.07x. Polymorphic inlining alone is +2.5 to 5%; the rest is the loops
  and the redundancy. Typescript plus or minus 3%.
- **Compile time:** inlining +5 to 15% in build and emit on
  inlining-heavy code; peeling +3 to 8% on loop-heavy compiles, +1% on
  Typescript; W3 plus or minus 2%, offset by W1's later savings. This is
  the stage that must make its case, within stage 2's gain (D7).
- **Gate:** DeltaBlue, Richards, RayTrace, Crypto and NavierStokes at two
  placements and four JIT offsets (`V8BENCH_JITPAD`, `V8BENCH_JITSEED`,
  `V8BENCH_JITLINE`); `TestJITCompileLinear` for W3's walk; `-jitcompile
  10` per phase, a phase over +10% put to the maintainer;
  `TestJITCompileWork` with the reason recorded; test262 with
  `-conformance.jit`, every function entered by OSR under
  `QJS_JIT_STRESS`.
- **Takes up:** performance-plan.md's "Inlining sizes calibrated to V8's
  bytecode".
- **Status:** not started.

### Stage 5: the baseline tier

10 to 14 weeks, 12,000 to 18,000 lines, high risk. Depends on stages 1
and 2.

- **Builds:** first its design, in internal/jit/README.md's style; then
  the stencils and routines from nativegen for both architectures, the
  compiler, the pc map, the baseline's call and return variant on a frame
  record designed once for both tiers, which stage 6's call convention
  uses; deopts and host exits resuming in baseline code; OSR from the
  interpreter to the baseline; the budget and the flush counting baseline
  code; then, once the baseline passes its gate, V8's tier-up policy:
  `jitBudgetForFeedback`'s 940 made 8 x length, the baseline's entry
  point, as V8's feedback allocation starts Sparkplug, the delay bit, the
  back-off and the raise at a cache's change. The estimate is wide for the exits' correctness at every pc Go
  returns to, and for generators.
- **Gain:** Typescript about 1,530 to 1,330 ms (1,220 to 1,450): the
  interpreted share's 150 to 230 ms recovered at 1.5 to 2x, and runs after
  deopts and exits continuing natively, which meets goal 2 with margin.
  ssr's first render at or below the interpreter's 35.5 ms, from 65;
  Octane's Box2D, PdfJS and RayTrace 6 to 22% faster on their first runs;
  the composite +0.5 to 1% (RegExp's 10% interpreted).
- **Compile time:** baseline compiles at 0.25 µs an instruction or less,
  about 15 to 25 ms over Typescript's warm functions; optimizing compiles
  unchanged unless D4's experiment raises the threshold.
- **Gate:** the baseline's compile speed against the kill gate of 0.5 µs;
  `TestRegNativePlainSameWithJIT`; a differential test running every
  register operation in baseline code against the handlers, on both
  architectures; test262 with `-conformance.jit`, every function
  baseline-compiled at its first call; Typescript's, ssr's and Octane's
  first runs; the interpreter no slower. The baseline's code at least
  1.5x the interpreter's on those first runs before the tier-up policy
  switches on; then each of its parts -- 8 x length, the delay, the
  back-off, the raise -- timed against what was before it on the first
  runs and the suites, and kept only where it gains.
- **Takes up:** performance-plan.md's "The tier-up's budgets".
- **Status:** not started.

### Stage 6: the call path

6 to 8 weeks, 6,000 to 8,000 lines, medium risk. Depends on stage 5's
frame record.

- **Builds:** the routines, reused from the baseline, for the optimizing
  tier; the stubs' ABIInternal form; native calls of baseline and
  interpreted callees, callees on a budget of their own, the native
  generic construction and calls of typed leaves; W4, the call
  convention.
- **Gain:** the composite +2 / +5 / +8%, the middle level's +1 to 5%
  (*estimate*: Go's work and the crossing are 1 to 14% of the suites) and
  W4's +1.3 / +2.0 / +2.9%; Typescript -3 to 5%.
- **Compile time:** emit and encode -10 to 20% as cold code becomes
  routine jumps; W4 emit -1 to 3%.
- **Gate:** Are We Fast Yet no slower with callees on their own budgets
  (it was 15% slower); `TestRegNativeCallHConvention`; the marking rule's
  tests for every routine that stores.
- **Takes up:** performance-plan.md's "A construction Go makes from native
  code" and the fixed registers for `abi.GoStub`'s operands, under "Calls
  of Go".
- **Status:** not started.

### Stage 7: the object model, dependencies and numbers

10 to 14 weeks, 10,000 to 14,000 lines, medium to high risk. Depends on
stage 1's lattice.

- **Builds:** c1, in-object values; shape dependencies, and W1's constants
  through them; c2's int32 and double elements, with int32 speculation for
  indices and arithmetic. It lands as the 32-byte object round did, by
  docs/optimizing.md's rules: one change, then its regressions. On amd64
  its int32 phis stay within the budget of five until stage 7b frees
  registers.
- **Gain:** the composite **+16 / +28 / +49%**, the largest stage; at the
  middle, cumulative from today, Crypto 2.27x, Richards 1.64x, DeltaBlue
  1.79x, NavierStokes 1.63x, EarleyBoyer 1.70x, RayTrace 1.32x. The
  interpreter gains on every property read and loses nothing on arrays or
  stores. Typescript -2 to 4%.
- **Compile time:** build +2 to 5% for the kinds' cases; W1 0 to 2%
  cheaper; emit -2 to 5% for the guards dependencies remove; a small cost
  at install for the dependency lists.
- **Gate:** the interpreter's property and array micro-benchmarks at two
  placements, none slower; test262 whole, for the transitions; the fuzzer
  under kind transitions; `TestIntendedInlining`, since a larger object is
  how inlining was lost before (AGENTS.md).
- **Status:** not started.

### Stage 7b: the backend

3 to 5 weeks, 2,000 to 4,000 lines, low risk. Needs nothing of stage 7
and may go before it; the projection counts it after.

- **Builds:** W5, W6 and W7 (above).
- **Gain:** the composite +1.6 / +3.3 / +6.8%, more of W6 once stage 7's
  int32 phis want registers; Typescript small.
- **Compile time:** neutral: optimize +3 to 5% for `thread`, emit -2 to
  3%.
- **Gate:** `compilediff native` showing only the intended instructions;
  `internal/jit/verify`'s encoders; Richards, Crypto and NavierStokes at
  the JIT offsets; `-jitcompile 10` per phase.
- **Status:** not started.

### Stage 8: allocation and final shapes

6 to 8 weeks for bare pools, folding and W8, and a spike of 2 weeks for
the refill-ahead helper; medium risk. Depends on stage 7's layout and
shapes.

- **Builds:** the Go-heap allocation above, with W8's final shapes at the
  take.
- **Gain:** with cores to spare, the composite **+7 / +15 / +30%**; at the
  middle, cumulative, RayTrace 2.04x, EarleyBoyer 2.49x, Splay 1.48x,
  DeltaBlue 2.04x. The low end is bare pools, folding and W8 alone; the
  high end needs the helper. At GOMAXPROCS 1, bare pools with c1 alone
  +6 to 9%. Typescript -1 to 2%.
- **Compile time:** neutral, W8's adds no longer built against its
  bookkeeping.
- **Gate:** the four allocating suites at 12 threads and at GOMAXPROCS 1,
  CPU and wall time; RSS no higher than the buffers' bound; the helper's
  threads under the one-P policy.
- **Status:** not started.

### Stage 9: strings and regular expressions

3 to 5 weeks, 3,000 to 4,000 lines, medium risk. Depends on stage 6's
routines.

- **Builds:** the regular expression glue, `replace`'s results and short
  concatenation as routines, longer strings as parts.
- **Gain:** RegExp 1.15 to 1.4x, Splay 1.08 to 1.2x (*estimate*); the
  composite +2.7 to 6.7%, more on the M5 Max. Typescript small.
- **Compile time:** small; routines in place of inline glue.
- **Gate:** the regular expression and string micro-benchmarks in both
  tiers; the matcher's step-budget tests.
- **Status:** not started.

### Stage 10: the engine heap, a spike and a decision

The spike 4 to 6 weeks; the project, if taken, 9 to 18 months
(*estimate*). Depends on stage 8.

- **Builds:** plain objects, arrays and closure cells in a non-moving heap
  of our own, StickyImmix as above, every other kind in Go behind
  handles, collection deferred while an unaudited Go frame is on the
  stack. It measures Splay, EarleyBoyer, RayTrace and Havlak with the JIT,
  at 12 threads and at GOMAXPROCS 1; what the handles and the `base +
  offset` arithmetic cost the interpreter; and the young collections' cost
  and fragmentation against the copying prototype's. The decision covers
  int32 fields (W9) and the property-access and barrier shares that only
  8-byte Values and an inline barrier remove.
- **Gain,** if taken, over stage 9: the composite +7 / +10 / +17% with
  cores to spare (the heap's +13 to 15% less what stage 8 took, plus W9's
  +0.6 to 3.0% and those shares, *estimate*); about +30% at GOMAXPROCS 1;
  8-byte Values throughout; the interpreter 10 to 15% faster on
  allocation-heavy code.
- **Compile time:** 2 to 6% cheaper (*estimate*), the keep pass and the
  marking paths gone against root maps' metadata.
- **Gate:** the maintainer's go or no-go on the spike's numbers (D1).
- **Status:** not started.

### Order

| Stage | Depends on | Serves most | A good place to stop |
|---|---|---|---|
| 0 | | goals 4, 5 | yes: correctness, parity, the budget |
| 1 | 0 | 2, 6 | yes: background by default, deopts disciplined |
| 2 | 1 | 6, 2 | yes: goal 2 likely met at its middle |
| 3 | 0 | 1 (Earley) | |
| 4 | 2 | 1 | |
| 5 | 1, 2 | 2, 3, start-up | yes: goal 2 met with margin, every first run faster |
| 6 | 5 | 1, 2 | |
| 7 | 1 | 1 | yes: the largest step, the composite about 1.5x |
| 7b | | 1 | |
| 8 | 7 | 1 with threads, 3 | yes: the composite about 1.8x at the middle |
| 9 | 6 | 1, the M5 Max | |
| 10 | 8 | 1, one-P hosts | the decision |

Stages 3 and 4 may go before stage 2 if the composite is wanted first;
stage 4 before stage 2 only with its compile cost accepted meanwhile.
Stage 7b may go before stage 7. The baseline comes after the first
composite stages because goal 2 may be met at stage 2, and because it is
the largest risk; if stage 2 leaves goal 2 unmet, the baseline moves up
to follow it (D10).

## The projection

Each class's share of native time, times native code's share of the
wall, times an efficiency -- the share of its measured gap a real
implementation closes, 0.6 low, 0.75 middle, 0.9 high -- is taken off a
suite's time, the classes split between the stages as above. Stages 1, 3,
5, 9, 10, stage 6's middle level and stage 8's refill factors keep their
own estimates. A suite is the geometric mean of its benchmarks, and no
benchmark passes 90% of the way to V8. Every figure here is an
*estimate*.

**The V8 composite, Ryzen 5 3600** (7,883; the goal 15,800). The suites'
columns are the middle's speedups, cumulative: Richards, DeltaBlue,
Crypto, RayTrace, EarleyBoyer, RegExp, Splay, NavierStokes.

| After stage | factor | low | middle | high | the middle by suite |
|---|---|---:|---:|---:|---|
| today | | 7,883 | 7,883 | 7,883 | |
| 0, 2 | 1.00 | | | | |
| 1 | 1.00-1.02 | 7,883 | 7,962 | 8,041 | |
| 3 | 1.03-1.05 | 8,149 | 8,288 | 8,422 | EB 1.39 |
| 4 | 1.05-1.11 | 8,550 | 8,925 | 9,372 | 1.12, 1.33, 1.07, 1.05, 1.44, 1.01, 1.04, 1.07 |
| 5 | 1.005-1.01 | 8,593 | 8,992 | 9,466 | |
| 6 | 1.02-1.08 | 8,788 | 9,443 | 10,226 | 1.21, 1.40, 1.11, 1.12, 1.55, 1.06, 1.10, 1.11 |
| 7 | 1.16-1.49 | 10,199 | 12,053 | 15,257 | 1.64, 1.79, 2.27, 1.32, 1.70, 1.09, 1.12, 1.63 |
| 7b | 1.02-1.07 | 10,361 | 12,446 | 16,302 | 1.79, 1.84, 2.37, 1.35, 1.75, 1.09, 1.14, 1.69 |
| 8 | 1.07-1.30 | 11,051 | 14,343 | 21,182 | 1.79, 2.04, 2.37, 2.04, 2.49, 1.09, 1.48, 1.69 |
| 9 | 1.03-1.09 | 11,385 | **15,172** | 23,129 | 1.79, 2.04, 2.37, 2.04, 2.49, 1.41, 1.80, 1.69 |
| 10, if taken | 1.07-1.17 | 12,156 | **16,721** | 26,963 | 2.20, 2.25, 2.52, 2.24, 2.74, 1.51, 1.97, 1.80 |

Without the wave (W1 to W9), the same model's middle is 12,811 after
stage 9 and 13,882 after stage 10.

**Sensitivity to the efficiency,** at the middle:

| Efficiency | after stage 9 | after stage 10 |
|---:|---:|---:|
| 0.65 | 14,100 | 15,400 |
| 0.75 | 15,200 | 16,700 |
| 0.85 | 16,500 | 18,300 |
| 1.0 | 19,000 | 21,500 |

**The confidence that the composite doubles,** from the spread: under 20%
by the end of stage 7, 35 to 40% by stage 9, about 50% by stage 10.

**The M5 Max** (20,395; the goal 40,800), by the same factors: about
39,300 at the middle after stage 9 and 43,300 after stage 10, the range
after stage 9 about 29,500 to 59,800. The M5 Max's Go-bound suites gain
more from stages 8 and 9 than the factors credit. The classes are
IR-level and carry to arm64, but for the registers, which arm64 has, and
instruction selection; arm64 was not measured.

**Typescript, one run, ms** (Ryzen 5 3600; the interpreter 1,565, Node
460; goal 2 is met below the interpreter):

| After stage | low (best) | middle | high (worst) | goal 2 |
|---|---:|---:|---:|---|
| today, background | 1,713 | 1,713 | 1,713 | no |
| 0 | 1,713 | 1,713 | 1,713 | no at 32 MiB, better at the default |
| 1 | 1,600 | 1,640 | 1,680 | no |
| 2 | 1,470 | 1,530 | 1,600 | at the middle |
| 3, 4 | 1,450 | 1,530 | 1,640 | at the middle |
| 5 | 1,220 | 1,330 | 1,450 | yes, 0.78 to 0.93x the interpreter |
| 6 | 1,170 | 1,280 | 1,410 | yes |
| 7 to 9 | 1,110 | 1,230 | 1,370 | yes |

Typescript's compile CPU at the end: 466 ms to 230-320, a compile 0.40 to
0.55 ms, the runtime goroutine's share 30 to 60 µs, which meets goal 6 at
the low end.

**Plainly:**

- Goal 2 is met by stage 5 with margin, and probably at stage 2's
  middle: it is a tiering problem, and the design meets it head on.
- Goal 1 needs the wave, and probably the engine heap. Without the wave
  the middle is 12,800 to 13,900; with it, 15,200 (1.92x) after stage 9
  and 16,700 (2.12x) after stage 10, so the middle crosses 2x only in
  stage 10. It crosses earlier if the classes close 80% of their gaps or
  more, and at the high end in stage 7b. The largest steps are stage 7
  (+28% at the middle) and stage 8 (+15%); stage 4 is +8%, most of it the
  loops and the redundancy, not the inlining.
- At GOMAXPROCS 1, what a host running a runtime per core sees, goal 1 is
  further off without the engine heap, which is worth about +40% there.
- On the M5 Max, 39,300 at the middle after stage 9 against 40,800, and
  43,300 with the engine heap.
- Time (*estimate*, the stages' weeks summed): goal 2 at its middle after
  stage 2, 2 to 3 months in, and with margin after stage 5, 6 to 10
  months in; stage 9 ends 13 to 19 months in; goal 1 at the middle then
  needs the engine heap's spike and 9 to 18 months of its own.

## The decisions

The maintainer took these on 2026-11-26.

| | Decision | Why |
|---|---|---|
| D1 | Doubling the composite needs the wave and probably the engine heap (about 50% by stage 10's end, 35 to 40% without it). Stage 10's spike is funded when stage 8 ends, and the heap is decided on its numbers, not begun before stages 4 to 8 show where the goal stands. Its collector is StickyImmix, non-moving by default, stopping the world per runtime, collection deferred while an unaudited Go frame is on the stack | the heap is the one design that removes mallocgc and 16-byte Values and lets the marking rule and keep cells go, but it is the largest and riskiest step and the others pay without it |
| D2 | The JIT budget's default is 64 MiB, with lowered programs dropped and the flush when control returns to the host | every measured program fits, the baseline adds code, RSS follows use, and the WebAssembly engine's default is the same |
| D3 | Background compilation is the default, with the one-P policy and `WithJITBackground(false)`, after stage 1's gate | Typescript is 13% faster in the background now, and the one-P policy removes the case that held it back |
| D4 | The optimizing threshold stays at V8's 400 x length when the baseline lands; 2x and 4x are then tried on Typescript, ssr and Octane's first runs, and the winner kept | V8 keeps Maglev at 400 over Sparkplug, and other engines' higher ratios are a measurement to make, not a premise |
| D5 | The 64-deopt cap is removed for the deopt discipline | V8 has none; the per-site and per-function discipline is what keeps a function from thrashing |
| D6 | Elements kinds, int32 and double, for elements only and never for Values, gated on the interpreter's array micro-benchmarks | the tagged-int experiment's cost was in Values; arrays of numbers stored as numbers are V8's Smi and double elements |
| D7 | Stage 4's compile cost is accepted up to stage 2's gain, a phase at most 10% over stage 2's figures with W3's and W1's savings counted beside it; beyond that, case by case | inlining's gain is real, but compile cost is the one regression no suite shows |
| D8 | The refill-ahead helper is a spike in stage 8, landed only if it wins wall time at 12 threads and costs nothing at GOMAXPROCS 1 | it is threads a host did not ask for, worth only with cores to spare |
| D9 | No process-wide sweeper at first: the flush when control returns to the host; a sweeper if hosts with pooled runtimes need it | the flush costs no API and covers the runtimes that run |
| D10 | The order as listed, the baseline moved up to follow stage 2 if stage 2 leaves goal 2 unmet | goal 2 may be met by stage 2, and the baseline is the largest risk |
| D11 | The maintainer runs the V8 suite on the M5 Max at each composite stage's end, the per-suite ratios against the table under The goals | timing stays on the Ryzen, and the M5 Max's Go-bound suites weigh differently |
| D12 | CacheIR-style feedback and trial inlining are the next project's | the snapshot as data takes CacheIR's cheap half now, and trial inlining needs the baseline first |
| D13 | The JavaScript engine adopts the collector first; the WebAssembly engine's GC objects follow, after this project | the heap matters far more to JavaScript, and little real-world WebAssembly depends on the GC proposal yet; the collector is designed for both, so the follow-up needs no redesign |

## The risks

| Risk | Stage | Mitigation |
|---|---|---|
| The gains are estimates past the measured shares; stages 7 and 8 carry most of the projection, and each class's efficiency decides goal 1 (0.65 to 0.85 moves the end from 15,400 to 18,300) | 7, 8 | each stage measured before the next begins; the projection made again after stages 4 and 7 |
| The classes' shares come from sampled instructions with skid; RegExp's and Earley's native classes are estimates, DeltaBlue's figures scaled from 80.7% coverage | the projection | the time ratios bound the totals; profiles again after stage 4 |
| Dependencies and constant fields invalidate code on a store; a site that keeps storing could thrash | 7 | a watched slot leaves the caches' fast entries after one invalidation, as V8's field constness generalizes |
| OSR compiled apart adds compiles and a second code for each function OSR enters | 4 | V8 makes few (2 of Typescript's 609); counted by `-jitcompile` and the budget |
| The baseline is a third consumer of every operation, and call and return are the hottest handlers | 5 | stencils from the same emitter; a variant for call and return, plain byte-identical; the differential test over every operation |
| More native code, whose faults in the arena kill the process | 5, 6, 7 | the encoders' verification, differential fuzzing, test262 with every function compiled at its first call |
| Arena code is not under the layout lock, and the stencils' and routines' op-cache placement varies | 5, 6 | JITPAD, SEED and LINE placements in every timing; routines in Go's text under an anchor |
| The background default brings nondeterminism to hosts, helper threads, and CPU when cores are busy | 1 | the one-P policy and the take-back; tests and tools pinned to the spot; the opt-out |
| Elements-kind transitions are where V8's bugs live | 7 | one-way transitions, the fuzzer, test262 |
| Compile cost creeps back, as with `ebe73f44`'s spill classes | every stage | the compile gates per stage |
| 64 MiB times many idle runtimes | 0 | the flush on return; the sweeper (D9); an eighth of `WithMemoryLimit` |
| A Go release changes what is done by hand: the ABIInternal convention, Go 1.27's loop-invariant hoist | 6, 7 | `TestRegNativeCallHConvention`, `hoistcheck` |
| The engine heap: 9 to 18 months, use-after-free as a new class of bug | 10 | the spike first; the deferral that turns a missed root into memory growth; a heap verifier and stress modes |

## How this document is kept

- **A stage's landing updates its status,** with the measured gain
  beside the projected one -- the composite, Typescript, the compile
  phases -- the tool, the suite and the machine by CPU model, and the
  commits. A part of a stage that lands updates the stage's status the
  same way.
- **What lands moves into internal/jit/README.md,** which says how the JIT
  works; this document keeps what is ahead, and a landed stage keeps here
  only its status line and its measured figures.
- **A change of direction** -- a decision taken again, a stage reordered,
  a part rejected on its numbers -- updates this document in the same
  commit, with why.
- **The projection is made again** after stages 4 and 7 with what was
  measured, and the confidence with it.
- An item of performance-plan.md that belongs to a stage is marked
  *(JIT project)* and links to it.
