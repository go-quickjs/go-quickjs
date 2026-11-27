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
- **The hot functions.** `hotdiff` and `go tool nm` on the register
  interpreter's hot set: the loop, `runReg`; the calls, `rhCallW`,
  `regEnterW`, `regCallSlow`, `regRunFD` and `callObject`; the native
  dispatcher's Go side, `nativeRun`, `nativeLoop`, `nativeDrive` and
  `nativeCallbackRun`; and its assembly,
  `nativeEnter`, `nativeCallH`, `nativeStoreRef` and the handler blocks
  (`regNativeHandlers`, `regNativeFeedbackHandlers`). Their sizes and
  addresses must match the base. A displacement that changes in an
  instruction of the same length is fine. A call that appears where code was inlined is not: `runFD` called
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
  code that does less than the interpreter's slow path leaves the object
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
- **Alignment within a placement group.** The placements shift six groups
  of code by multiples of 32 bytes, starting at `callObject`, `jitLeafGo`,
  `runReg`, `rhCallW`, `nativeDrive` and the assembly after the closures,
  each at 0 and at 32 mod 64 in half of the eight (the placements tool's
  package documentation has the design), so a function that a change moved
  by an odd multiple of 32 relative to its group has its 64-byte
  alignment flipped at every placement. Compare the test's hot functions'
  addresses mod 64 on both sides before calling it a cost.
- **The handlers' layout.** The register interpreter's loop has no
  cases to move: its Go handlers are functions laid out one after
  another, and the native handlers a block in `handlerOps`' order. The
  placements move the block whole, after the closures, and do not vary
  the layout inside it; a handler added moves those after it in its file
  or the block, which is why new ones are appended
  (docs/register-interpreter.md, "Code layout").
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
plain, with the JIT, with the WebAssembly engine and with both, for the
toolchain the targets were recorded with -- and `internal/cmd/layoutpad`,
which `go generate ./internal/vm` runs,
builds `internal/cmd/v8bench` as the targets were recorded and sizes the
pads until every anchor is at its target. `TestLayoutLock` fails when one
has drifted.

The present targets were recorded from the tree as the stack interpreter
and the tree tier were removed, where the code was then, not chosen by
measurement: the timing of the register interpreter across placements
decides them, and a target it shows to be a bad one is recorded anew.

What it pins, and how. Each lever is a constant of
`layoutpad_GOOS_GOARCH.go`, `layoutpad_jit_GOOS_GOARCH.go`,
`layoutpad_wasm_GOOS_GOARCH.go` or `layoutpad_jit_wasm_GOOS_GOARCH.go`; every pad is
byte stores, 7 bytes of code each, so that it fills any whole number of
32-byte slots.

| Lever | The pad | Moves | Pins |
|---|---|---|---|
| A I E T JP C D F L B J N P | `layoutPadX`, a function in a file sorted where the gap opens | every function after it in file order, then the closures and the generics | `joinSize` (the built-ins), `bigParts` (`bigint.go`, the BigInts' arithmetic), `firstElem` (`builtins*.go`), the JSON encoder's `encode` (`builtins_json.go`, T), JSON.parse's parser, `parseJSON` (`builtins_json_parse.go`, JP), `asciiCaseMemo` (`callsite.go` and `casetables.go`), `descriptorObject` (`define.go` on), `iterResult` (`generator.go`), `add` (`vm.go`'s arithmetic, `arith`), `callObject` (with `callDirect` and `runFD` after it), with the JIT `jitLeafGo` (J), `nativeRun` (`native_run.go`: `nativeApply`, `nativeNew`, `nativeBuiltin`), and `regCall` (P: the register calls' entries, `regCallSlow` and `regRunFD`) |
| BM TA CB JB JU JN JS VT VA PR SH | the same, positions the hot-function rule (below) asked for | every function after it to the next position | a Map's and a Set's lookups, `(*jsMap).find` (`builtins_map.go`, BM), a typed array's element reads, `(*typedArrayData).getElem` (`builtins_typedarray.go`, TA), the array methods' calls of their callbacks, `(*regCallback).call` (`callback.go`, CB), with the JIT its BigInt leaves, `jitBigLeafMake` (`jit_bigint.go` on, JB), a string builder's appends, `jitAppend` (`jit_builder.go`, JU), native code's entries and its framed calls of Go, `jitRegWhole` and `jitRegGoCall` (`jit_native.go`, JN), and its calls of host functions, `jitHostCall` (`jit_ssa.go`, JS), the register handlers in Go, `rsAdd` (`reg_table.go`, VT), a value's tests and accessors where they are not inlined (`value.go`, VA), a property's read, `getProp` (`vm_property.go`, PR), and a shape's root, `rootFor` (`vm_shape.go`, SH) |
| JE JO | `layoutPadX`, the same, after a JSON function that changes grow or shrink | every function after it to the next position | the JSON encoder after `encodeObject`, `(*jsonEncoder).encodeElements` first, with `encodeProps`, `writeKeys` and the escapers `jsonEscapeASCII` and `jsonEscapeWide` after it (`builtins_json_elements.go`, JE), and the parser after `parseValueAt`, `(*jsonParser).parseObject` first, with `parseArray`, the strings and `parseNumber` after it (`builtins_json_parse_object.go`, JO), at `c906a894`'s layout: a first form of KI-89's nesting count, 96 bytes more of `parseValueAt` and 64 less of `encodeObject`, moved the code after them, and a build that moved only that code was 10% slower on `json_stringify_unicode` and 4% on `json_parse_decimals` at every placement |
| KO CC BU CN WE | `layoutPadX`, the same, after the JIT's files (`keyo_layoutpad.go`, before `keyorder.go`), before `memory.go` (`mem_layoutpad.go`), and in the files the native dispatcher's Go side and the call path were split into for them (`native_run_b_layoutpad.go` before `native_run_builtin.go`, `native_run_n_layoutpad.go` before `native_run_new.go`, `reg_run_call_e_layoutpad.go` before `reg_run_call_enter.go`) | every function after it to the next position | what follows the JIT's files, `reserveFunctionProtoKeys` first; a string's concatenation, `concat`, past the memory meter's walk of the frames; the native dispatcher's calls of built-ins, `nativeBuiltin`, and its constructions and applies, `nativeNew` and `nativeApply`; the frames `rhCallW` enters, `regEnterW` and `regLeafRun`: at the offsets of a build without the JIT in every configuration, so that the JIT's files and the code its hooks add before them move none of it (docs/jit-design.md, "The JIT built in and off: parity held by test") |
| Q W U V | `layoutPadX`, the same, in the register interpreter's files | every function after it to the next position | the register interpreter's loop, `runReg` (Q), the call path after it, `rhCallW` (W), its operations in Go, `regBinImm` and `regBinary` (`reg_runs.go`, U), and its handlers, `newRegSwitchTable` (`reg_switch.go` and `reg_table*.go`, V, at `2f05d3fc`'s layout) |
| Y K | `layoutPadX`, the same, in the register interpreter's files | every function after it to the next position | the generators and async functions, `regGenOf` (`reg_generator.go`, Y: `regAsyncStart`, `regAsyncRun`), and the cold handlers, `regColdInit` (`reg_table_cold.go`, K: await's `rhAwait`), at `16f8b5e9`'s layout |
| O | `layoutPadO`, before `vm_object.go` | `vm_object.go` and every function after it | what follows the literal constructors, `newErrorObject`, and with it the property definitions after it (`setOwnRaw`, `appendProp`, `setArrayLength`, `vm_property.go`, `vm_shape.go`), at `16f8b5e9`'s layout; the functions before the constructors in `vm_object.go` (`newObject`) move with the pad |
| R S | `layoutPadX`, the same, after the register interpreter's files | every function after it to the next position | a replace's loop, `regExpReplaceIn` with its templates after it (`regexp_abstract.go`, R), and the string builder, `partsBuilder` (`string.go`, S) |
| Z | `layoutPadZ`, after every file | the closures, and the generics the last files call | the closures of the named functions, `init.func1` first, at `41a137be`'s layout |
| G*x* M*x* CZ | a generic a position's pad, or a method at it, calls; a closure in a generic the last file calls | the generics plain functions or methods meet first, from that file back; the closures of generics and every generic after them | CZ: the generics after the native handlers' block, `appendGrown` (an array's growth by push) first, at `41a137be`'s layout; GK: the string appends, `strAppendOp` and `strAppend`, after `typeofKind`; MVT: the regular expression leaves, `(*regexpLeaf).match` and the match results before it, after `rootOfKeys`; the other G and M: nothing at present |
| DH DW DG | 8-byte pointer variables, 8- and 16-byte variables without | the zeroed data after them, which the linker sorts by size | `runtime.mheap_`, `runtime.writeBarrier`, `runtime.gcController` |
| X | `layoutPadX`, a function in `internal/regexp/aa_layoutpad.go`, its constant in that package's own configuration files | every function of package regexp, and what follows it up to vm, whose levers absorb it | the matcher, `(*matcherASCII).run`, and with it its `search` and the searches for a unit (`indexUnit`, `skipUnitsASCII`) |
| XJ | `layoutPadXJ`, a function in `internal/jit/aa_layoutpad.go`, its constant in that package's own configuration files | every function of package jit, and what follows it up to vm, whose levers absorb it | native code's calls of Go, `ExpandGoCall`, at `8097c77b`'s layout, in the builds with the JIT |
| H | `layoutPadH`, an assembly function of whole 32-byte slots of int3 bytes in `native_a_layoutpad_*.s`, one file a configuration, sorted after the JIT's assembly and before `native_<arch>.s` | the native handlers' block, and vm's `init` and the closures of generics after it, which CZ absorbs | `regNativeHandlers`, and with it `nativeEnter` and `nativeCallH` before it, at the same distance in every build: the feedback handlers, in a build with the JIT, come after it (`native_handlers_feedback_<arch>.s`) |
| HJ | `layoutPadHJ`, the same in `jit_l_layoutpad_*.s`, sorted after the JIT's BigInt leaves' assembly and before `jit_leaffast_<arch>.s`, in the builds with the JIT | the JIT's leaf stubs and the assembly after them up to H | `jitLeafFast` and `jitGoFast`, native code's leaf and Go call stubs, which every closure added had moved |
| HT | `layoutPadHT`, the same in `native_ta_layoutpad_*.s`, sorted after `native_handlers_<arch>.s` and, with the JIT, `native_handlers_feedback_<arch>.s`, and before `native_typed_<arch>.s` | the typed code's handlers and what follows them up to CZ | `regNativeTypedHandlers`, which lay 98 KB past H, after the whole native handlers' block |

What each lever cannot move:

- **Function pads** move a whole range of files: between two positions the
  code keeps its own layout, and a change inside a range moves what follows
  it there. A position is a file name sorted where the gap must open
  (`defg_layoutpad.go` between `deferred.go` and `define.go`), and every one
  costs its pad. The built-ins before `define.go` were one range, pinned
  at its start only, and no placement pad lies before them, so the
  placements never vary them: when the files were renamed to plain names,
  the files that came to sort among them moved the built-ins' hot loops
  2 KB and made `array_indexOf` 23%, `json_round_trip` 10% and
  `string_upper` 7% slower at every placement. E (`builtin_layoutpad.go`,
  before `builtins.go`) and C (`calls_layoutpad.go`, before `callsite.go`)
  hold those loops at their earlier offsets. R (`regexp_a_layoutpad.go`,
  before `regexp_abstract.go`) and S (`strh_layoutpad.go`, before
  `string.go`) do the same for a replace's code, which the renames had
  put after files new to its place: `regexp_replace_dollar` was 4 to 8%
  slower at every placement, where the base with the same code shifted
  by 32 to 96 bytes was not. I, F, L, N, P and U hold at `3ecb8200`'s
  offsets the code the renames and those positions moved (`bigint.go`,
  `generator.go`, `vm.go` from `add`, `native_run.go`, `reg_call.go`,
  `reg_runs.go`):
  `forward_construct` was 18%, `generator_next` 14%, `bigint64_divmod`
  13%, `bigint256_arith` 9% and `string_upper` 8% slower there. P holds
  `regCall`, after `native_run.go`, in every configuration: with the JIT
  it had been in N's range, 74 KB past `nativeRun`.
  V holds `reg_switch.go` and `reg_table*.go` where `2f05d3fc` had them,
  not `3ecb8200` (see Generics below). The positions trade some rows for
  others, as the code between them moves: over four placements against
  `2f05d3fc`, the micro-benchmarks' geometric mean is 0.996, and
  `async_call` is 9% slower at every placement (its promises,
  `promise.go`, lie in N's range; 221 ns at `3ecb8200`, 190 at
  `2f05d3fc`, 207 now), `array_spread` 4.6%, `spread_call` 4.2%,
  `asm_table` 7.4% and `int_to_string` 2.6% slower at most of them.
  T (`builtins_j_layoutpad.go`, before `builtins_json.go`) holds the
  JSON encoder at `5ef0d50b`'s offsets: the WebAssembly engine's merge
  grew `builtins_atomics.go` and `builtins_intl.go` by 160 bytes, which
  moved it, and `json_round_trip` was 7% slower with the JIT at every
  placement. One position holds one stretch: `encodeObject` grew by 96
  bytes in the same merge, with the check for a host's opaque proxies,
  and moved what follows it -- `encodeElements`, the string writers,
  the parser and the built-ins up to C -- 96 bytes past `5ef0d50b`'s
  offsets, which made `json_stringify_escapes` 15% and
  `json_stringify_unicode` 38% slower with the JIT. T with those at
  their offsets, the encoder 96 bytes early, had given
  `json_round_trip` back but left `json_stringify_records` and
  `json_stringify_small` slower; the fix was in the code:
  `encodeObject`'s BigInt wrapper case, which always throws, went out
  of line, and the function is as long as it was. That put the parser
  and the built-ins after it back at `5ef0d50b`'s offsets too, where
  `json_parse_decimals` was 6%, `json_parse_exponents` 4% and
  `typed_f64_reduce` 5% slower at every placement than 96 bytes on: JP
  (`builtins_json_p_layoutpad.go`, before `builtins_json_parse.go`, the
  parser moved out of `builtins_json.go` for it) holds them at
  `a2126155`'s, 96 bytes past. `json_stringify_unique` is held by
  neither: its `encodeProps` and `writeKeys` lie between
  `encodeElements` and `appendJSONString`, in the stretch whose
  offsets `json_stringify_escapes` and `json_stringify_unicode` want,
  and it is 7% slower with the JIT than 96 bytes on, as at `5ef0d50b`.
  Y, K and O hold at `16f8b5e9`'s offsets code that changes around it
  moved where no position was: the literal constructors' rewrite moved
  `vm_object.go`'s code after them 608 bytes back, and
  `array_hole_length_decr` (`setArrayLength`) and `object_computed`
  (`appendProp`, `shapeAdded`, `defineOwnProp`), their code unchanged,
  were 5 to 7% slower at every placement or nearly; changes to the
  register calls' entries moved `reg_generator.go` and to the handlers
  before it `reg_table_cold.go`, and `async_call` was 9% slower. A pad
  must sit between files, so O, before `vm_object.go`, moves the
  functions before the constructors (`newObject`) with it.
- **Closures** come after every function, in the compiler's order: the
  named functions' closures roughly in file order, then the init
  functions' in descending init order, then a group in reverse file order.
  Z holds them at their start, `init.func1`, against the functions before
  them: CZ's pad, which the last file's init calls, had moved them 32
  bytes. Closures against each other are not pinned, nor can a position
  pin them: a closure pad put in a position's file is laid out with the
  closures of the other pads, not before those of the file after it (a
  trial with one at every position put them all together, by
  `initTemporalPlainMonthDay`'s closures). Hot code in a closure is held by
  nothing, so it goes in a named function the closure calls: the typed
  arrays' callback methods, 0.5% and 0.2% of the plain build's samples,
  lay 82 KB past Z as `defineTypedArrayMethods.func17` and `func18`, any
  closure before them moving them, and are `typedArrayCallback` and
  `typedArrayReduce` in TA's range now, after `setElem`. The package's
  assembly -- the native handlers' block, its entry and exit stubs, the
  JIT's leaf stubs -- is laid out after the closures, so a closure added
  moves it, which H takes up.
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
  `newPolyObject`, `appendGrown`, the constructors' `chargeChild`). CZ,
  whose pad is among the generics' closures, which come first, moves
  every generic after it, and holds them against the native handlers'
  block before them, whose growth had moved them: the fused branches'
  handlers grew it 967 bytes, moved `appendGrown` 960, and made
  `array_push_args5_single` 7% slower with the JIT at every placement.
  GK and MVT hold two groups against the generics before them: a G or
  M pad lands where the compiler's order puts it, not where its file
  sorts, so `layoutpad -show -all -generics` writes one at every
  position and shows where each lands, and a group gets the pad that
  lands right before it. Other generics are not held against each
  other, and so a function
  pinned at an older offset is not held against the generics it calls,
  nor the native handlers' block's time, which depends on where the Go
  handlers sit against the generics: code against code, which no single
  anchor covers. V put at `3ecb8200`'s offset `rsAdd` (`reg_table.go`), with the
  generic string appends it calls (`strAppend`, `strAppendOp`, laid out
  after the assembly) left at `2f05d3fc`'s, made `upvalue_string_append`
  29% slower than either; with no V, where U's pad left them,
  `forward_construct` was 30% slower than `3ecb8200`, its Go functions
  all at their offsets; so V holds them at `2f05d3fc`'s.
- **Data**: the levers place the zeroed data. The initialized data and the
  read-only data, among it the type descriptors `mallocgc` reads, are not
  pinned. A build's data depends on its build information as well -- VCS
  stamping, which a tree with changes makes longer, and cgo's settings --
  so the targets are of `CGO_ENABLED=0 go build -buildvcs=false` builds,
  and the placements tool builds so too. Built otherwise, the code is where
  the lock puts it and the data up to 32 bytes off.
- **Other packages and programs.** The lock pins package vm against the
  runtime in `v8bench`, and the regular expression matcher by X: a package
  is laid out where the packages linked before it end, so a change that
  grows one of those moves it -- `internal/bytecode`'s new operations moved
  the matcher 3,520 bytes and made the V8 suite's RegExp 3.5% slower at
  every placement -- and vm's pads, after it, cannot reach it. A pad in
  another package is an outer lever of `layoutpad` (its `outers`): a
  function pad in the file that sorts first there and the package's own
  constants. The regexp generics vm instantiates are vm's and move with
  its generics' levers. The other packages before vm -- go-intl, the JIT's
  compiler -- are not pinned, and the placements do not vary them either.
  Their order follows the order in which vm's files, in file-name order,
  first import them: renaming vm's files put `internal/bignum` and
  `internal/bytecode` before go-intl, moved the matcher 14 KB, which X
  took back, and `bignum`'s `writeSmall` 3 KB, which made
  `bigint_toString` 6% slower at every placement. `aa_imports.go` and
  `aa_imports_jit.go`, which sort before every other file and hold no
  code, import vm's packages in the order its files did before the
  renames, which keeps that order on Windows and Linux amd64; a package
  vm newly imports comes after them. The functions that fill vm's long
  map literals (`map.init.N`) are laid out before A's pad too, with the
  code no pad of vm's moves: `jitSnapOps`, a map of 4,352 bytes of such
  code in the builds with the JIT, is an array for that, and the room it
  gave back put A's range on its page again after `internal/jit/compile`
  had grown past A's pad. In another
  program (`qjs`, an embedder's) vm starts elsewhere and only its layout
  against itself holds.
- **The WebAssembly engine.** A build with it is laid out anew: the
  functions only the bridge reaches (`wasm/wasmjs`'s calls of vm) are
  linked there, which moves vm's code against itself, and the runtime's
  code and data move too, by different amounts (`memmove` 192 bytes,
  `growslice` 96, a 16-byte variable more before `writeBarrier`). So
  the lock has a configuration for `v8bench` built with `quickjs_wasm`,
  and with `quickjs_jit,quickjs_wasm` (`internal/cmd/v8bench/wasm.go`
  installs the namespace in every runtime, as qjs does), whose anchors
  in vm have the targets of the build without the engine: vm's hot code
  is where it is without it, and only the runtime around it differs.
  Without them, `json_stringify_unicode` with the JIT took 50 ns in the
  WebAssembly build where it took 37 without, the same code at other
  offsets. A data target such a build cannot reach is the nearest one
  that holds (`layoutlock.txt` says which).
- **The JIT.** The configurations with the JIT take plain's targets the
  same way, every code anchor plain has and, for jit, its data anchors
  (`TestJITConfigsAtPlainTargets`), and add their own for the JIT's code
  (J, JB, JU, JN, JS, XJ, HJ): a build with the JIT runs the interpreter,
  the JIT off, on the sets a build without it does. With targets of their
  own, string appends and BigInts were 7 to 16% and generators 4 to 9%
  slower with the JIT built in and off, at every placement. What the JIT
  adds is kept out of the interpreter's ranges: its files sort together,
  KO holds what follows them, a function only the JIT calls lives in its
  files (`bigRelease`, the marking forms of the slow paths' additions) or
  where a range ends, and a hook is a test and a call, not a body inlined.
  `hotdiff -parity` checks the result.
- **Assembly** is laid out after the closures of the named functions and
  before vm's `init` and the generics' closures, in file order. The
  register interpreter's native handlers are one assembly block
  (`native_handlers_<arch>.s`, nativegen's), so that any closure
  added or grown anywhere in the package moved all of them: one round's
  closures moved the block 672 bytes (608 in one of its branches, which
  made Octane's RegExp and the generator tests slower). H pins
  `regNativeHandlers`; between it and its pad are only `nativeEnter` and
  `nativeCallH`, which every exit to Go runs. The feedback handlers, which
  a build with the JIT alone has, sort after the plain handlers, in HT's
  range: before them, they had put the two stubs 4.6 KB further from the
  handlers with the JIT built in than without, on other sets of the op
  cache. The pad is a whole number of slots, at least
  one, so a target the block is already at costs 4 KB of pad. It is
  written for amd64 alone, as the targets are.
- **Native code**, which the JIT lays out at run time, is not affected; its
  placements are the JIT's own (`V8BENCH_JITPAD`, `V8BENCH_JITLINE`).
- **Another toolchain or platform** lays the runtime out anew: the targets
  are one Go release's, the tool refuses another (and the test skips), and
  other platforms build no pads (`layoutpad_other.go`). New targets are
  recorded with `layoutpad -record -targets FILE` at the base chosen as the
  layout to keep, the tool built beforehand.

The register interpreter is a small loop over a table of handlers, a
function an operation (docs/register-interpreter.md, "The loop"): Q pins
the loop, `runReg`, and W the call path after it, `rhCallW`. A handler
is a function of its own, so an edit to one moves no other's code but
those after it in its file; there is no switch whose cases a lock would
hold, and so no golden file of case offsets and no lever inside the
loop. The native handlers' block is anchored by H (above); inside it the
handlers are laid out in `handlerOps`' order, new handlers last, and
arm64's block is not tuned at all.

The pads are never called and their variables never read: one init calls
them behind a variable that is never true, which keeps the linker from
dropping them, and every variable is the same size whatever the levers are
but the data levers' own. They cost their size and only grow: an anchor a
change moves back by 32 bytes takes a pad 4,064 bytes longer, and a pad's
own function moves what follows it, so a position whose target is where its
code was takes a pad of 4 KB. A pad that goes so past a page moves
everything after it 4 KB against everything before it: the same offsets
modulo 4 KB, and a page further from the packages linked before vm and
from the runtime, which Zen 2 still tells apart. Function.prototype's
key reservation (`8b5d6339`) took E from 32 stores to none, three
`keyOrder` calls (`c7c7830a`) took `firstElem` 96 bytes past that, and
E went to 4,011 bytes: `json_stringify_decimals` was 39%,
`json_stringify_records` 15% and `json_stringify_indent` 11% slower at
every placement than with E short, their code and its offsets
unchanged. With the
reservation in `keyorder.go`, after T, E is 20 stores and the three are
back. A pad near none is worth a look before a change grows the code
before its anchor. The targets file records each code anchor's address
too (`@ADDR`), and a build that has an anchor on another page -- its
address over 4,096 -- fails the regeneration and `TestLayoutLock`, which
name the first anchor that moved, its pad and its size, and the anchor
before it that did not: the code in that pad's range grew past what the
pad had, or the pad wrapped. Move what grew there out of the range, after
the anchor, as `b5df63ea` did. A move that is meant -- code that grew by a
page or more, where nothing can be moved back -- is recorded with
`go run ./internal/cmd/layoutpad -pages`, which pads as `go generate`
does and writes the new addresses; say so in the commit. A regeneration
puts each anchor on its recorded address while its pad can take up the
change, past a page if need be, so an anchor only moves a
page when the code before it grows past its pad's room. Where `-pages`
records a lost page anew, it records the one that leaves the pad its
reserve, 2 KB, and 8 KB for XJ and CZ, whose ranges -- the JIT's
compiler, the native handlers' block -- grow in most rounds, and moves
the anchors after it as far: the next such move is that much growth
away. `-reserve XJ,CZ` gives the pads named their reserve where they
have not lost a page.

The lock as first made, against `db306c0e`'s layout and with the tree
tier's anchors, was 49 KB of pads, 60 KB of binary.

The lock and the placements: a placement build adds its own pads to the
locked tree and so varies what it varies around the locked layout, both
labels by the same amounts as long as both are locked or one is the base
the targets were recorded at.

**Adding a position.** The rule: no function with 0.2% or more of a
configuration's CPU samples over the micro-benchmarks lies more than
8 KB past the anchor of the range it is in. Code between a range's
anchor and the next pad keeps its own layout and moves with every change
before it there, so a hot function far into a range moves with them: at
1d08a62d `jitHostCall` lay 251 KB past J, the JIT's leaf stubs 485 KB
past Z, every closure moving them, and the array methods' callbacks
140 KB past JP. `go run ./internal/cmd/layoutpad -hot` lists what breaks
the rule for this machine's configurations (`-config` picks them): it
builds each as the targets are measured, runs its micro-benchmarks under
the CPU profiler (`-hotruns`, 10), and names each hot function over
`-hotdist` (8,192) bytes past its anchor, with its range, exiting 1 if
there is one. A profile of a build of one's own does as well: `go tool
pprof -top` of it beside `go tool nm` of the same binary. Such a function
wants a position: a file in `positions` (`internal/cmd/layoutpad`)
sorted right before the function's file, and an anchor on the function
written in `layoutlock.txt` as `anchor LEVER new SYMBOL` in each
configuration it is hot in. The next regeneration records the new
anchor where its pad, at its least, leaves it, so that the position
holds the layout from there: the code between the new pad and the next
one moves by the pad's 32 bytes, once, and is compared then. A new pad
has no room, so a position added before another new one moves that
one's anchor too: write it as new again (TA and CB were, for BM). The
WebAssembly configurations take the targets of the configuration
without the engine, as their other anchors do, written as numbers; then
`-pages -config` those configurations records their addresses.

**Equality functions.** The compiler makes an equality function
(`type:.eq.*`) for a struct or array type that a comparison, a map key or
an interface's `==` needs and that memequal cannot compare -- a padding
hole, a float, a string or an interface among its fields -- and lays it
out with the code of the package that first needs it: a new one moves
every function after it up to the next pad. Three moved hot code in two
days, each found by timing (`testConfig`, `strAppend` 64 bytes;
`[64]primAbsentEntry`, `strAppend` and `strAppendOp` 128 bytes and
`upvalue_string_append` 12.6% slower; regcomp's `tjump`, `wtf8.Scan` and
the code after it up to regexp's pad 64 bytes). `internal/vm/layouteq.txt`
lists each configuration's, and a regeneration and `TestLayoutLock` fail
on one it does not list, naming where it is laid out and the functions
whose code calls it (none: a map's key or an interface's comparison
needs it). Make the type need none -- no padding hole, or a
non-comparable `_ [0]func()` field where nothing should compare it --
or, where the new one is meant, record the lists with
`go run ./internal/cmd/layoutpad -eq`, beside `-pages`, and say so in
the commit.

When a change lands, run `go generate ./internal/vm`. A change that adds
hot code where no lever reaches it should add a position or an anchor
(`layoutlock.txt`), its target recorded at the base it is compared with.
What the lock leaves free, with the measurement of the lock against the
layout it pinned, is in `docs/performance-plan.md` ("Measurement and
tooling").

## Builds and the machine

- **Build both sides in the same checkout.** A base built in another
  directory differs in its data: paths in the binary, build information.
  `construct_new_target` measured 575 against 500 ns that way and 0.85 to
  0.98 of main's built in the same checkout. The placements tool builds
  each label in the checkout it is run in.
- **One copy of a build is one draw too.** On Windows two copies of the
  same bytes have timed `json_stringify_ints` 26 and 46 ns, each the same
  every run. Most of it was where Windows loads each file, in steps of
  64 KB the lock does not hold: the placements tool and the matrix clear
  DYNAMICBASE and HIGH_ENTROPY_VA in their windows/amd64 builds' PE
  headers and check that each loads at its image base, 0x140000000, as a
  Linux build loads at its link address (`-fixedbase`, package
  `internal/cmd/v8bench/internal/fixedbase`). A copy at that base can
  still keep a speed of its own -- one of eight took 48 ns on
  `json_stringify_decimals` where seven took 35, until copied again --
  most likely for where its file lies in memory, so they also run each
  process from a fresh copy (`-freshcopy`). A row that moved by a tenth
  between two single builds is timed again from fresh copies before it is
  believed.
- **Alternated rounds do not average a file's draw out.** Windows gives
  an image its base once for its file, and every run of that file loads
  there until the system lets the image go, so two builds run directly in
  alternated rounds compare two load addresses as many times over. Two
  files of one program, their code and data the same bytes, timed
  Octane's Typescript in the interpreter at 1,541 and 1,522 ms, medians
  of six such rounds on a Ryzen 5 3600, and looked like a regression of
  the commits between them. Four fresh copies of the slower file took
  1,502 to 1,516 ms, and both files at the image base, each process from
  a fresh copy, 1,520 and 1,519. On Windows a build is timed only that
  way, at its image base and from a fresh copy -- the placements tool's
  and the matrix's `-fixedbase` and `-freshcopy` -- never as the go
  command made it.
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
