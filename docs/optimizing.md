# Optimizing go-quickjs

This is how an optimization is measured, judged and landed. It is written
from the object-layout work that made an `Object` 56 bytes to 32: the
first version kept 20% fewer bytes and was faster on every suite, yet
took four rounds to land, because a dozen micro-benchmarks were 5 to 30%
slower than main. Most of those regressions were not in the code the
change was about. They came from the inliner, from where the linker put
code and data, from the Go runtime, and from the JIT's feedback meeting a
new kind of object. Each rule below is one of them, written down so that
the next change finds it in hours, not days.

AGENTS.md's Validation section describes the tools. This document says
when to use which and what their results mean.

## What a change must show

- **A net gain, measured.** Suites, the micro-benchmarks' geometric mean,
  bytes and allocations, and CPU time with the collector's included. A
  change that is merely no slower does not land: it costs code, review
  and future layout noise for nothing. If the gain is eaten by the
  change's own cost -- a JIT pass's compile time, say, which
  `v8bench -jitcompile N` measures by phase -- make that cost smaller
  before deciding the idea does not pay.
- **No large regression anywhere.** A geometric mean that improves does
  not pay for one pattern that got 15% slower: a program that is all that
  pattern sees only the 15%. A test 3% or more slower at every placement,
  plain or with the JIT, is a regression until its cause is fixed or
  proven not to be the change (see "Proving a cause" below), or the
  maintainer accepts it.
- **Totals as well as minimums.** The micro mode reports the fastest run
  of each test. `prop_in` with the JIT looked 1.1x main's by its fastest
  run and was 5.5x by its total: most runs left native code every
  iteration, and one reached the good code. A test whose minimum is level
  and whose total is not has a warm-up or deoptimization problem.
- **The same answers.** test262 plain and with `-conformance.jit` under
  JIT stress, and a regression test for each mechanism the change adds or
  fixes. The tests that pin a mechanism -- a count of shapes after 40,000
  iterations, a count of Go calls, a function compiled within so many
  runs -- are what keep the next change from breaking it silently.

## Order of work

1. **Write the benchmarks first.** For the pattern the change targets and
   for the ones it may hurt. The object-layout change was judged at first
   by the suites, which have few objects on fresh prototypes; the eight
   micro-benchmarks written for it afterwards (`object_create_fresh`,
   `class_factory`, `scope_chain`, ...) showed it 1.3 to 1.6x main's on
   exactly that.
2. **Profile both sides before guessing.** A merged CPU profile of the
   base and the change, diffed; for native code, `jitprof` over
   `-jitcode`. Most wrong turns in this work were hypotheses made before
   a profile.
3. **Screen fast.** One build a side, only the tests the change touches,
   `-n 3`. It tells the direction, not the verdict: a single build swings
   5 to 25% from code placement alone.
4. **Judge over placements.** `placements compare -parallel -k 4 -micro`,
   plain and `-jit`, then the suites the same way. Confirm a finished
   batch with all eight placements before it lands.
5. **Fix, then measure only what the fix touches.** A full run after
   every fix costs hours; a full run at the end of a round is enough.

## Reading a slower test

Go down this list in order. After the layout lock, the first five are
about the code, the rest about where it and its data are.

- **Is the layout lock current?** A change compared without its pads
  regenerated (`go generate ./internal/vm`, see "The layout lock" below)
  is compared at wherever it moved the hot code. With the lock current,
  what is left of the list below is what the lock does not pin.
- **Is it the code?** The profile says where the extra time is. If every
  function of the test got uniformly slower, suspect placement; if one
  did, read it.
- **Inlining.** Go inlines a function of cost 80 or less. A change that
  grows a small function past it, or grows what it inlines, turns a few
  instructions into a call in every caller. No test fails, and the
  benchmarks show it as a few per cent scattered across unrelated tests.
  `TestIntendedInlining` guards the hot helpers; add what a new hot path
  relies on. To see what a change lost, diff the sorted `can inline` lines
  of `-gcflags=<pkg>=-m` between the two builds. Keep a growth path (the
  call that grows a table) out of an inlined fast path: split it into an
  out-of-line "make room" and an inlined store.
- **The hot functions.** `hotdiff` and `go tool nm` on the seven of
  AGENTS.md: their sizes and addresses must match the base. A
  displacement that changes in an instruction of the same length is
  fine. A call that appears where code was inlined is not: `runFD` called
  `newObject` once `newObject` stopped inlining, and every instruction
  after the call moved.
- **Representation checks.** When a change moves a fact into the shape,
  a guard on the shape implies it. Code that still loads and compares it
  does work for nothing: native code loaded each holder's prototype after
  checking holders' shapes, three dependent loads and a branch a holder,
  where the VM's own cache checked shapes alone.
- **The JIT's feedback.** A new kind of object meets every cache-fill
  path. One that counts a fill but remembers nothing makes the JIT treat
  the site as unfed and deoptimize at each run. A Go fallback from native
  code that does less than the tree tier's fallback leaves the object
  where native code's probe never matches. A heuristic that guesses a
  usage pattern must recognize the opposite pattern early: one that
  treated each class's first object as a singleton reset RayTrace's
  tier-up budgets and compiled 12 functions in its first run where main
  compiled 20. `-jitreport`, Go-call counts (`jitCountGoCalls`), the SSA
  listing's exits and the warm-up trace find these.
- **The Go runtime.** Smaller objects change size classes and how many
  share a span. The runtime's list of a span's weak pointers is walked
  from the start for each one added, so many weak pointers to small
  objects, made in increasing address order, cost far more than to
  larger objects handed out highest address first.
- **The closure block.** Package vm's closures are laid out after all its
  functions, as one block, and the placements move that block whole: they
  never vary the distance between two closures. A test whose hot code is
  a closure late in the block is slower or faster at every placement
  because of what other closures did. `Math.clz32` was 1.22 to 1.31x
  main's for that reason; as a named function it is 0.92x. Hot code goes
  in named functions, where the placements vary it.
- **Alignment within a placement group.** The placements shift groups of
  functions by multiples of 32 bytes, so a function that a change moved
  by an odd multiple of 32 relative to its group has its 64-byte
  alignment flipped at every placement. Compare the test's hot functions'
  addresses mod 64 on both sides before calling it a cost.
- **Native code's layout inside a function.** Moving a whole native
  function by lines changes little; moving code within its lines, or
  changing distances inside it, does. The JIT's placements move code by
  whole lines (`JITPAD`, seeds) and start each function's code at an
  offset into its line (`V8BENCH_JITLINE`, 0 to 56 bytes by 8), which
  moves its loops -- each at a line's start within its function -- and
  branches within their lines and 32-byte fetch windows: a test slower at
  some of those offsets and faster at others is placement, judged by its
  mean over them. A change that makes a function's native code longer
  moves its blocks against each other, which no offset varies. To tell
  that from a cost, perturb the other side's code -- a few bytes inside the
  function -- and see whether it moves to the same figure: main with one
  `xorpd` before each `cvtsi2sd` went from 7.65 to 8.6 ns on
  `typed_u8c_forEach`, the branch's 8.4.
- **Package data.** A new package variable, or one moved from initialized
  data to bss by being built by a function, moves the Go runtime's hot
  variables (`runtime.writeBarrier`, `gcController`) within their cache
  lines: 1 to 2% on allocation-heavy tests. Compare their addresses mod 64,
  then diff the data and bss symbols in address order to see where the
  offset changes. Prefer literals to initializer functions, and fold a
  new `sync.Once` into an existing one. The placements give the data a
  phase of 0 or 32 bytes, so that each label's builds take both.
- **Read-only data.** The type descriptors `mallocgc` reads on every
  allocation live in rodata, which no placement varies yet. Two builds
  with identical code and type descriptors 32 bytes apart took 211 and
  192 ms on `string_template`. When identical instructions run at
  different speeds, diff the rodata symbols' starts.
- **A degraded base.** A test faster on the base in a full run than when
  run alone hints at a cache or budget filling up on the base and
  switching it into a cheaper, worse mode. The 32-byte objects' base had
  its shape tree fill during `class_factory` in a full micro run, after
  which it made no layouts for constructors; the change, which gives
  collected prototypes' shapes back (`17b21643`), never filled it, and
  looked slower there for being right.

## Proving a cause

"It is layout" is a claim like any other and needs evidence:

- **A pad-only proof.** Pad the change's build so that the code the test
  runs sits at the base's addresses again, without changing an
  instruction, and show the test goes level. Or the reverse: pad the base
  to the change's addresses and show it goes slower.
- **Ranges, not single points.** "It moved between 78.7 and 86 ns across
  pads" proves nothing when the base's range is 73 to 78.5: the change is
  above the base at every pad.
- **Collector pacing** needs collector numbers: GC counts and cycle times
  per test from `GODEBUG=gctrace=1`, on both sides.
- **Data placement** needs the data's addresses on both sides and a build
  that restores them.
- **When nothing moves a gap**, list what was ruled out, with numbers, and
  say so. It is not layout until an experiment makes it level.

## The layout lock

Most of the "slower at every placement" findings above were where the
linker put code and data, not the code. The layout lock takes that out of
a comparison: generated padding, dead code and data, puts package vm's hot
code at fixed addresses modulo 4 KB (the op cache's sets) and the Go
runtime's hot variables at fixed offsets in their cache lines, whatever a
change does around them. `internal/vm/layoutlock.txt` names each anchor
and its target for each configuration -- windows/amd64 and linux/amd64,
plain and with the JIT, for the toolchain the targets were measured with
-- and `internal/cmd/layoutpad`, which `go generate ./internal/vm` runs,
builds `internal/cmd/v8bench` as the targets were measured and sizes the
pads until every anchor is at its target. `TestLayoutLock` fails when one
has drifted.

What it pins, and how. Each lever is a constant of
`layoutpad_GOOS_GOARCH.go` or `layoutpad_jit_GOOS_GOARCH.go`; every pad is
byte stores, 7 bytes of code each, so that it fills any whole number of
32-byte slots.

| Lever | The pad | Moves | Pins |
|---|---|---|---|
| A D R V B S T J Z | `layoutPadX`, a function in a file sorted where the gap opens | every function after it in file order, then the closures and the generics | `joinSize` (the built-ins), `descriptorObject` (`define.go` to the tree's builder), `buildTreeSkipping` (the tree's builder and slow paths, `arithSlow`), `executeAt`, `callObject`, `runTree`, `callTree`, `logicalJumps` or with the JIT `jitLeafGo`, and the closures at `binaryNode.func1` |
| GV | a generic `layoutPadV` calls | the generics plain functions meet first, from `vm.go` back | `rmwStmtHook` and the tree's generic nodes after it: `strAppend`, `upvalueAdd`, `litNodeOf`, switch, typeof, the regexp leaves |
| MA MV MS | a generic that a method at the position calls | the generics methods meet first, from that file back | the method values (`upvalueAdd.node-fm` ...), `dataViewLeaves` and the DataView leaves, `polyReread` and object creation's |
| CZ | a closure in a generic the last file calls | the closures of generics | `strAppendNode.func1` and the generic tree nodes' closures |
| DH DW DG | 8-byte pointer variables, 8- and 16-byte variables without | the zeroed data after them, which the linker sorts by size | `runtime.mheap_`, `runtime.writeBarrier`, `runtime.gcController` |

What each lever cannot move:

- **Function pads** move a whole range of files: between two positions the
  code keeps its own layout, and a change inside a range moves what follows
  it there. A position is a file name sorted where the gap must open
  (`defg_layoutpad.go` between `deferred.go` and `define.go`), and every one
  costs its pad. The built-ins before `define.go` are one range, pinned at
  its start only.
- **Closures** come after every function, in the compiler's order: the
  named functions' closures roughly in file order, then the init
  functions' in descending init order, then a group in reverse file order.
  The Z pad moves the block whole, as the placements do; closures added
  before `binaryNode.func1` are absorbed, but closures against each other
  are not pinned.
- **Generics** come last, by when Go first instantiates them: it reads the
  bodies of a package's functions last first, then its methods', and
  instantiates a generic where a body first meets it, then what that
  generic's body meets. A pad a file's function calls is laid out between
  the generics the files after it meet first and those of the files before
  it, so the G and M levers pin a group at a time. A generic added inside a
  group moves what follows it there (`sortIndexed`, `densify` and
  `spreadShaped` in the round the lock was made for), and the generics
  package-level variables' initializers or other types' methods meet first
  are groups no lever reaches (shape budgets' `setRoot` and `firstRoot`,
  `newPolyObject`, `appendGrown`, the constructors' `chargeChild`).
- **Data**: the levers place the zeroed data. The initialized data and the
  read-only data, among it the type descriptors `mallocgc` reads, are not
  pinned. A build's data depends on its build information as well -- VCS
  stamping, which a tree with changes makes longer, and cgo's settings --
  so the targets are of `CGO_ENABLED=0 go build -buildvcs=false` builds,
  and the placements tool builds so too. Built otherwise, the code is where
  the lock puts it and the data up to 32 bytes off.
- **Other packages and programs.** The lock pins package vm against the
  runtime in `v8bench`. The packages before vm -- the regular expression
  matcher, go-intl, the JIT's compiler -- are not pinned, and in another
  program (`qjs`, an embedder's) vm starts elsewhere and only its layout
  against itself holds.
- **Native code**, which the JIT lays out at run time, is not affected; its
  placements are the JIT's own (`V8BENCH_JITPAD`, `V8BENCH_JITLINE`).
- **Another toolchain or platform** lays the runtime out anew: the targets
  are one Go release's, the tool refuses another (and the test skips), and
  other platforms build no pads (`layoutpad_other.go`). New targets are
  recorded with `layoutpad -record -targets FILE` at the base chosen as the
  measured-best layout, the tool built beforehand.

The pads are never called and their variables never read: one init calls
them behind a variable that is never true, which keeps the linker from
dropping them, and every variable is the same size whatever the levers are
but the data levers' own. They cost their size and only grow: an anchor a
change moves back by 32 bytes takes a pad 4,064 bytes longer, and a pad's
own function moves what follows it, so a position whose target is where its
code was takes a pad of 4 KB. The lock as made, against `db306c0e`'s
layout, is 49 KB of pads, 60 KB of binary.

The lock and the placements: a placement build adds its own pads to the
locked tree and so varies what it varies around the locked layout, both
labels by the same amounts as long as both are locked or one is the base
the targets were recorded at.

When a change lands, run `go generate ./internal/vm`. A change that adds
hot code where no lever reaches it should add a position or an anchor
(`layoutlock.txt`), its target recorded at the base it is compared with.
How the lock was made and measured: `docs/performance-plan.md`.

## Builds and the machine

- **Build both sides in the same checkout.** A base built in another
  directory differs in its data: paths in the binary, build information.
  `construct_new_target` measured 575 against 500 ns that way and 0.85 to
  0.98 of main's built in the same checkout. The placements tool builds
  each label in the checkout it is run in.
- **Single builds are for profiles, not verdicts.**
- **An idle machine.** Check per-process CPU before a comparison, and run
  nothing else during one. `-parallel` gives each label its own level-3
  group; compare a label with itself first on a machine new to it.
- **Room for the build.** A full `go test ./...` in parallel can hit
  Windows' commit limit (`VirtualAlloc ... errno=1455`, `fatal error: out
  of memory`) with little free on the paging file's drive: run it with
  `-p 4`, and clear the Go build cache when the drive fills -- between
  builds, not during them.
- **No elevated tools** (hardware counters, kernel profilers) without the
  maintainer's approval.

## Landing

- **Bug fixes found along the way land first, by themselves**, each with
  its test: they are fixes to main, not part of the optimization, and an
  optimization that is held back should not hold them back. A part that
  belongs to another layer -- a compiler analysis a VM change needs --
  goes in a commit of its own.
- **Tool fixes land by themselves too**, so that both sides of every later
  comparison measure the same way. That includes the benchmarks' own
  sources: a base without a change's new micro-benchmarks runs different
  code. Their extra globals changed a constant in every loop's global-cell
  code, two bytes longer, and made `array_some` and others look 16% slower
  on the change's side.
- **Rebase when main moves**, and measure against the base the change will
  land on.
- **Fold fix-ups into the commits they fix** before the merge; each commit
  that remains says what it does, why, how it was checked, and what it
  measured.
- **Record what was tried and failed**, with its numbers, in the commit
  body or `docs/performance-plan.md`, so that nobody spends a day on it
  again.
