# Known issues

Found by a review of the engine, the standard library and go-intl in
September 2026, after v0.5.0. Each issue was reproduced unless it is marked
*plausible*. They are ordered by how much harm they allow, and are fixed in
that order. The commit that fixes an issue names it: `git log --grep KI-01`.

- **P0**: a script can corrupt the engine, crash the host process, or reach
  past a boundary it should not.
- **P1**: a script can defeat a limit -- a deadline, a memory bound -- or make
  the host hang or leak.
- **P2**: a wrong result that code can observe.
- **P3**: a small divergence from the specification or from Node.

Paths are relative to this repository; `go-intl:` paths are in
[go-intl](https://github.com/go-quickjs/go-intl).

## P0: corruption, crashes, escapes

### KI-01 Suspended generators and async functions share frame state with later calls
- **Where:** `internal/vm/generator.go` restores `handlers` and
  `openUpvalues` into a pooled frame and saves them back sharing the same
  backing arrays. The next call at the same depth truncates and appends to
  them (`vm.go`).
- **Effect:** a suspended function's catch handlers and captured variables
  are overwritten. The result is a Go panic that closes the runtime, or wrong
  results.
- **Repro:** an async function that awaits inside `try/catch`, while another
  function with a `try` runs at the same depth; or closures over `let i`
  across `yield`, which give `[3,1,2]` for `[0,1,2]`.
- **Status:** fixed. The frame lets go of the slices when the generator
  suspends or ends.

### KI-02 `WithMemoryLimit` is not enforced
- **Where:** `internal/vm/runtime.go`. `accountMemory` has no callers.
- **Effect:** with a 1 MiB limit, a script can allocate gigabytes of arrays,
  strings and buffers. qjs's `--memory-limit` does nothing either.
- **Status:** fixed. The runtime measures its own heap by walking it from its
  roots, once the process has allocated a quarter of the limit since it last
  did, and before a buffer, a repeated or padded string, or a join that
  doubles a rope. A script over the limit stops with `ErrMemoryLimit`.
  Native loops that allocate without ever reaching an interrupt check are
  KI-16's.

### KI-03 Strings have no maximum length
- **Where:** `internal/vm/string.go` (`Concat`), and `padStart`, `padEnd` and
  `repeat` in `builtins_string.go`.
- **Effect:**
  - A rope's length wraps: doubling a string past 2^63 gives length 0 or a
    negative length.
  - Flattening a huge rope, or `"x".padStart(2**40)`, ends the process with
    a fatal out-of-memory that nothing can recover from.
  - `padStart(Infinity)` panics, which closes the runtime.
  - `repeat`'s cap counts code units, not bytes.
  - On 386, a length past 2^31 turns negative.
- **Expected:** V8 throws `RangeError: Invalid string length` above 2^29 - 24
  code units.
- **Status:** fixed. Strings are limited to V8's length, with V8's message, at
  every place one can grow past it: `+`, `concat`, templates, `repeat`,
  padding, the joins, `String.raw` and `replace`. Typed-array `join` and a
  string pattern's `replace` also stopped building a rope node per piece,
  which cost many times the text they held.

### KI-04 Unbounded Go recursion ends the process with a fatal stack overflow
- **Paths that recurse in Go with no depth limit:**
  - proxy `get`/`set`/`has` forwarding, through a cycle or a long chain
    (`builtins_proxy.go`, `vm_property.go`)
  - calling a chain of bound functions (`callObject`)
  - an async generator's request queue
    (`finishAsyncRequest → pumpAsyncGenerator`)
  - `JSON.stringify` re-entered from `toJSON`, and `JSON.parse` from a
    reviver
  - `Array.prototype.flat` over proxies
  - `IsCallable` and `isConstructor` on a proxy chain
- **Also:** `WithMaxCallDepth(400000)` exhausts the Go stack before the
  depth limit is reached.
- **Effect:** `fatal error: stack overflow`, which `recover` cannot catch.
- **Expected:** Node throws `RangeError: Maximum call stack size exceeded`.
- **Status:** fixed. Recursion that no frame counts -- proxy forwarding, bound
  functions, JSON's nesting and its reviver, `flat` -- is counted against a
  limit sized to the Go stack, as the call depth now is too, whatever a host
  asks for. A proxy records whether it is callable and a constructor when it
  is made, and an async generator's queue is served in a loop.

### KI-05 A typed array's `sort`/`reverse` go through replaceable `Array.prototype` methods
- **Where:** `internal/vm/builtins_typedarray.go` (`toSorted`, `toReversed`,
  `sort`, `reverse`).
- **Effect:** a replaced `Array.prototype.sort` receives the engine's
  internal array and can put a hole in it. The hole reaches `toNumber`, which
  recurses forever: a fatal stack overflow.
- **Also:** a replaced `Array.prototype.reverse` turns a typed array's
  `reverse` into a no-op.
- **Status:** fixed. The four methods work on the view's own values, and
  `toNumber` refuses a kind it cannot convert rather than recurring.

### KI-06 Huge array-likes are materialised
- **Effect:**
  - `Array.prototype.slice.call({length: 2**32-1})` writes every hole out.
  - `unshift` on an array of length 2^32 - 1 interns an atom per index.
  - Both end the process out of memory.
- **Status:** fixed. A result counts the holes it is given and becomes sparse
  past a short run of them, and reading or deleting an index past 2^31 on an
  ordinary object makes no key for a name that was never one. Walking such an
  array-like still takes as long as it does in V8, which a deadline stops.

### KI-07 The atom table only grows
- **Where:** `internal/vm/atom.go`.
- **Effect:** every string used as a key is interned for the runtime's
  lifetime, even when it is only read. The same goes for every symbol key and
  every index at or above 2^31. A script that only reads `o["k"+i]` grows the
  heap without bound.
- **Status:** fixed for keys that are only read, tested or deleted -- by
  `[]`, `in`, `delete`, `hasOwnProperty` and `Object.hasOwn` -- which make no
  atom for a name that has none, unless a proxy or a module namespace on the
  chain has to be asked. A key a script writes keeps its atom for the
  runtime's life, as before; the memory limit counts the table.

### KI-08 ShadowRealm isolation escapes through `CallSite`
- **Where:** `internal/vm/stacktrace.go`. An anonymous native frame, such as
  the ShadowRealm wrapper, is skipped without hiding the frames past it.
- **Effect:** code inside a ShadowRealm that sets
  `Error.prepareStackTrace = (e, s) => s` gets the outer realm's `this` and
  functions through `getThis()` and `getFunction()`.
- **Status:** fixed. A trace made on one side of a ShadowRealm's boundary
  hides the frames on the other, both ways, and a built-in frame a trace
  leaves out hides its callers as one it shows does.

### KI-09 An exception from one runtime can be rethrown into another
- **Where:** `marshal.go` (`throwGoError`).
- **Effect:** a `*quickjs.Error` a Go function returns is rethrown as it
  is, even when it came from another runtime. The receiving script then
  holds the other runtime's objects and can run its code on the wrong
  goroutine. Since v0.5.0, `Call`/`Get`/`Set`/`New` return `*Error`, which
  makes this easy to hit.
- **Status:** fixed. An exception of another runtime crosses as an `Error`
  with its message.

### KI-10 `Intl.NumberFormat` given a numeric string with a huge exponent
- **Effect:**
  - `format("1e-100000000000")` ends the process out of memory, and
    `"1e-999999999"` allocates 2 GB.
  - `"-1e9223372036854775807"` overflows the engine's exponent and panics.
- **Expected:** V8 throws `Error: Internal error. Icu error.` when the
  leading digit is below 10^-999999999.
- **Status:** fixed. go-intl 4b0b612 counts the zeros instead of writing
  them, the engine holds an exponent where adding to it cannot overflow, and
  ICU's bound is a Node divergence: the standard formats the number, and
  `WithNodeQuirks` throws V8's `TypeError`.

### KI-11 Waiting on shared memory that grew after it was shared panics
- **Where:** `internal/vm/sharedmem.go`. `wait` and `waitAsync` index the
  memory by its length when it was shared.
- **Repro:** `sab.grow(16); Atomics.wait(new Int32Array(sab), 2, 0, 1)`
  after sharing an 8-byte growable buffer.
- **Effect:** index out of range; the runtime is closed.
- **Status:** fixed. Both read the memory at its current length.

### KI-12 A data race on the shared empty string
- **Where:** `internal/vm/string.go`. `codeUnits()` writes the `u16` cache
  of the process-wide `emptyString`.
- **Repro:** runtimes on different goroutines running
  `"".replace("", "$'")` race.
- **Status:** fixed. The empty string caches nothing.

### KI-13 Freezing the global object doesn't freeze `Intl` and `Temporal`
- **Where:** `internal/vm/builtins_intl.go`, `temporal.go`. The lazy
  accessor replaces itself with `setOwnRaw`, and its setter ignores `this`.
- **Effect:** after `Object.freeze(globalThis)`, reading `Intl` makes the
  property configurable again, and `globalThis.Intl = 5` succeeds.
- **Status:** fixed. Redefining the property, or freezing or sealing the
  global object, first makes it the data property it stands for; the getter
  replaces only its own configurable accessor; and the setter respects its
  receiver. Building `Intl` also stopped defining its constructors,
  `NumberFormat` and the rest, as globals.

### KI-67 Native code read a closure's template from a run since collected
- **Where:** `internal/vm/zzjit_ssa.go`. A closure native code does not
  make stood in a frame state as the address of its template in the
  constants of the closure the code was compiled from, one run of the
  program; the entry kept neither.
- **Effect:** a use after free, with the JIT, in any program run more than
  once, in one realm or several. Once the compiling run was collected, an
  exit read freed memory into the frame: `TypeError: 7.04892706e-316 is not
  a function`, or `fatal error: invalid pointer found on stack`.
- **Status:** fixed. The code keeps the template in a cell of its own, as it
  keeps string constants (`jitTemplateCell`).
  - Test: `TestJITMadeTemplateOutlivesItsRun`.

## P1: limits, hangs, leaks

### KI-14 Interrupts are turned into values
- **Where:**
  - `thrownValue` (`promise.go`) turns a cancelled context into a
    rejection.
  - `closeIteratorsIn` (`iterate.go`) drops the error.
  - The interrupt counter is re-armed after reporting, so `DrainJobs`'s
    per-job check rarely sees it.
- **Effect:** a script past its deadline, or a terminated worker, keeps
  running. `.then(spin).catch(again)` loops for good.
- **Status:** fixed. An interrupt is remembered once seen: every check after
  it reports it, no function starts, and the job queue stops. The host's
  next call starts afresh, without the jobs the stopped script left, and a
  `node:vm` timeout stops only the run it bounds.

### KI-15 A nested `Runtime.Eval` clears the outer deadline
- **Where:** `quickjs.go`. `evalIn` resets the context to nil on return, and
  drains the job queue in the middle of the outer script.
- **Effect:** a Go callback that evaluates, as `stdlib.Inspect` does,
  removes the caller's timeout.
- **Status:** fixed. A call made from inside a running script runs under the
  script's context as well as its own, restores the script's after it, and
  leaves the script's jobs for its turn.

### KI-16 Native work that a deadline cannot stop
- **Regular expressions:** an `exec` may take 100M steps with no interrupt
  check. Compiling is quadratic in group nesting (`internal/regexp`).
- **Loops without an interrupt check:**
  - `Array.prototype.fill` on an array-like (`arrayLike.set` never ticks)
  - `indexOf` on non-ASCII strings, which is also quadratic
  - `toUpperCase` of a huge string
  - `7n ** 9999999n`
  - `JSON.stringify` of 1e7 elements
  - `Intl.getCanonicalLocales` on a huge array-like
  - module linking, which is also quadratic
- **Status:** fixed. `fill` on an array-like, `JSON.stringify`'s walk and
  Intl's locale lists check for an interrupt; `JSON.stringify` is held to
  KI-03's longest string. A regular expression's match checks for one where
  the host can stop the script, and compiling one is linear, as are
  `indexOf` and `lastIndexOf`. A BigInt is held to V8's 2^30 bits, checked
  before the work where it can be. Linking modules is linear, with the
  loader asked once for each import. `toUpperCase` of a string as long as
  one can be, and BigInt `**` within the limit, take a few seconds that
  cannot be interrupted, as they take V8 somewhat less; that is accepted.

### KI-17 The regular expression step budget rejects ordinary patterns
- **Effect:** `/z/.test("a".repeat(6e7))` and `/a*z/.test("a".repeat(2e4))`
  throw `SyntaxError: regular expression is too complex`; Node returns
  `false`.
- **Fix:** an interruptible matcher (KI-16) removes the need for the budget.
- **Status:** fixed where the host can stop the script -- a context with a
  deadline, or an abort, as qjs always has: there a match has no budget and
  takes as long as it does in V8. A host that cannot stop the script keeps the
  budget, which is all that stands between a catastrophic pattern and a hang.

### KI-18 The job queue gives up after a million microtasks
- **Where:** `internal/vm/promise.go`.
- **Effect:** a legitimate loop of 1.1M `await`s fails with an uncatchable
  "the microtask queue did not drain".
- **Status:** fixed. The queue has no count; a chain that queues itself
  forever is stopped by the host's deadline, as a loop is.

### KI-19 A Go error that wraps a cancelled context can't be caught
- **Where:** `marshal.go`, since 98da02a.
- **Effect:** `throwGoError` matches any error wrapping `context.Canceled`
  or `DeadlineExceeded`, so an embedder's `http.Client` timeout ends the
  script instead of throwing an `Error`.
- **Status:** fixed. Such an error passes through uncatchable only when the
  runtime's own context has ended or it has been aborted.

### KI-20 `terminate()` doesn't stop a worker waiting in a host call
- **Where:** `stdlib/worker.go`. The worker's `Loop` context is its
  runtime's, not the terminate context.
- **Effect:** a program started with `execFileSync` runs to the end, and
  the worker posts again after it was terminated. Code after `close()` or
  `terminate()` runs until the next interrupt check.
- **Status:** fixed. A worker's loop context ends when it is terminated, so a
  program it runs is killed and nothing it posts after is delivered. A web
  worker's `close()` lets what is running finish, as the HTML standard has it.

### KI-21 `Loop.Close` doesn't end a `Run` that a worker holds
- **Effect:** the worker's exit notice is posted to the closed loop and
  dropped, so the hold is never released. `Run` waits until its own context
  ends.
- **Status:** fixed. `Run` and `RunUntil` return once the loop is closed.

### KI-22 BroadcastChannels of a closed runtime stay registered
- **Where:** `stdlib/messaging.go`. Only a worker's shutdown unregisters its
  channels.
- **Effect:** each channel keeps its runtime's heap reachable, and every
  later broadcast of that name queues on it without bound.
- **Status:** fixed. A channel stops being registered when its runtime's
  context ends.

### KI-23 Ports inside messages that are dropped are never closed
- **When messages are dropped:**
  - posting to a closed port
  - closing a port with messages queued
  - a worker ending with messages unread
- **Effect:** the ports inside those messages are left entangled with no
  owner, so a started peer holds its loop forever. Node closes them.
- **Status:** fixed. Every place a queue or a message is dropped closes the
  ports it carries.

### KI-24 A closed runtime's `waitAsync` waiters absorb notifies
- **Where:** `internal/vm/sharedmem.go`.
- **Effect:** asynchronous waiters stay in the list after their runtime
  closes. `Atomics.notify(i, 0, 1)` wakes a dead waiter instead of a live
  one, and each dead waiter keeps its runtime's heap reachable.
- **Status:** fixed. A runtime keeps its waiters, and `Close` takes them off
  their lists.

### KI-25 `Atomics.wait` timeouts above about 9.2e12 ms end at once
- **Where:** `internal/vm/sharedmem.go`. The conversion to a `time.Duration`
  overflows, so `wait(..., 1e300)` answers `"timed-out"` immediately.
- **Status:** fixed. A timeout past what a `time.Duration` holds is forever.

### KI-26 qjs's Ctrl-C can't interrupt a running callback
- **Where:** `cmd/qjs/main.go`. `Loop.Run` checks its context only between
  tasks.
- **Effect:** `setTimeout(() => { for (;;) {} })` can't be interrupted, and
  neither can `--timeout`.
- **Note:** Node can't interrupt such a callback on Ctrl-C either.
- **Status:** fixed. A loop's callbacks run under the context the loop runs
  under, so `--timeout` -- and qjs's Ctrl-C, which cancels the same context --
  stops one that never returns.

### KI-27 Operations whose cost grows quadratically
- **Strings:** concatenating a surrogate pair's halves flattens the whole
  string each time. *Fixed: the halves come off the ends of the ropes.*
- **Proxies:** creating a proxy walks the whole proxy chain. *Fixed with
  KI-04.*
- **`bind`:** the "bound bound ..." name is copied in full each time.
  *Fixed: the name is a rope, and messages show its first KiB.*
- **Intl** (go-intl):
  - `ListFormat` of many items
  - Thai and CJK word segmentation
  - `Segments.containing`
  - `Segmenter.segment()` also splits the whole string up front
  - *Fixed in go-intl 9b59e27, all but the last: a list is written from
    the front, and the breaks and boundaries are searched. Splitting up
    front costs one pass over the string per `segment()`, not more.*
- **Temporal:** a Chinese-calendar `until` over the whole date range steps
  one month at a time. Node hangs here too. *Fixed in go-intl v0.4.0: each
  month is balanced from the one before rather than from the first year,
  and the whole range in months takes a tenth of a second.*
- **Status:** fixed, but for `segment()` splitting up front, which is
  linear.

### KI-28 32-bit platforms truncate lengths and offsets
- **Panics on GOARCH=386:**
  - `[1,2,3][2**31]`
  - `a[2**31+5] = 1` and `a.length = 2**31`
  - `new ArrayBuffer(2**31)`
  - `Array.prototype.sort.call({length: 2**31})`
  - `String.raw({raw: {length: 2**31}})`
- **Wrong results:**
  - `fill` above 2^32 does nothing
  - `"a,b".split(",", 2**32-1)` gives `[]`
  - `lastIndex` above 2^32 wraps
  - `splice` on a length past 2^31 throws
  - a typed array offset or `set` offset of 2^32 becomes 0
- **Status:** fixed. The module builds for 386 and arm, and
  `TestLengthsPastAnInt32`, run with `GOARCH=386`, pins the cases. A string's
  longest there is V8's for 32 bits, 2^28 - 16. `sort` and `String.raw` of a
  length of 2^31 take long, but V8 runs out of memory on the first and grows
  without bound on the second.

### KI-65 A realm lives as long as its runtime
- **Where:** `internal/vm/realm.go`, what the runtime keeps of a realm.
- **Effect:** a realm a host had dropped was not collected, with the JIT or
  without it: of twenty realms made with `NewRealm`, each running one script,
  twenty were alive after two collections, and a host that made a realm for
  each request grew until it closed the runtime. A heap dump's paths named
  what held them:
  - the runtime's list of every realm's leaf built-ins (`leafFns`), which
    held each realm from the moment it was made;
  - the last realm's Math functions (`powFn`, `atan2Fn`, `hypotFn`,
    `randomFn`), which also left the JIT no intrinsics in the realms before
    it;
  - the layouts the runtime kept of iterator results, regexp results and
    arguments objects, and its table of the primitives' methods, each
    holding the last realm to use it, or the first;
  - its megamorphic caches, of reads, additions and the in operator, up to
    an entry's worth of realms each;
  - a registration's cleanup, which the collector holds whatever else is
    alive, and which held the registry, its callback, the callback's realm,
    and so the target that had to die for it to run;
  - with the JIT, entries' pools and feedback, the megamorphic cache and
    what its contexts last ran with.

  Shapes went on counting against the tree too: a literal's root, counted as
  its site first ran, was never given back, and a realm made for each run of
  a program filled the tree in some tens of thousands of runs.
- **Status:** fixed. What caches what a realm's code found is the realm's, and
  goes with it; a cleanup holds its cell weakly, and a registry is kept by the
  realm of its callback; a realm's literals' roots no longer stay in the tree's
  count (KI-66); the JIT's cache lets go of a realm none of whose entries ran
  between two of its looks, a second of the program's time apart
  (`jitFlushRealms`), and it is compiled again if it runs again.
  `TestDroppedRealmsAreCollected`, `TestRealmsPerRequestAreCollected`,
  `TestJITDroppedRealmsLeaveTheCache` and `TestShapeBudgetRealms` pin it. What
  remains is by design, as V8 holds what its caches hold weakly and this
  engine's caches cannot:
  - Code of another realm that has read a dropped realm's objects holds their
    layouts, and so the realm, in its sites' caches until they are filled
    again, one realm for each entry at most.
  - With the JIT, a dropped realm stays until the runtime next looks, which it
    does as it runs: a host whose runtime has gone idle keeps the last realms
    it ran, and a host making realms at a thousand a second keeps a second's
    worth.

### KI-66 Running one program again and again fills the shape tree
- **Where:** `internal/vm/vm_shape.go` (`siteRoot`, `shape.child`) and
  `internal/vm/zzz_shapebudget.go`.
- **Effect:** each run of a compiled program prepares its code anew
  (`prepare`), so each object literal has a new root and new shapes grown from
  it. Those are garbage once the run's closures are, but the tree's count keeps
  them, against the realm's `Object.prototype`, which in a runtime's only realm
  is never collected and so never gives them back. A script of two literals,
  run in one realm, reached 40,004 shapes at 10,000 runs and the 65,536 of
  `maxShapes` by 20,000. From then on no object is given a shared layout: each
  has a layout of its own, which no cache can remember, whatever it runs. A host
  that runs one `Program` for each request in one realm gets there, and so does
  one that calls `Eval` for each, with new bytecode each time; a realm for each
  request does not, its `Object.prototype` giving its shapes back (KI-65).
- **Status:** fixed. A literal's root and the shapes grown from it hold one
  another, and are garbage together, so they are counted against a budget the
  root holds (`chargeChild`), not against their prototype, and a root is out
  of the count once the code that made it stops running (`settleSites`):
  there is one for each literal of the code that lives, which is no growth
  the count is there to stop. So the two-literal script holds 32 shapes at
  2,000 runs, where it held 12,000, and at any number after. On top of that a
  `Program` makes no shape for a run after its first: a realm keeps a root for
  each of its literals, as the layout of its `Object.prototype` keeps the roots
  of other objects made on it, and each run's caches start from them
  (`zzz_rerun.go`), so that a run's objects have the layouts the last run's had
  and code compiled in one run holds for the next. The two-literal script's
  allocations fall from 17 to 7 a run. `TestShapeReruns`,
  `TestShapeRerunsAlternating`, `TestShapeFreshCodeRuns`,
  `TestShapeAfterReruns`, `TestProgramRerunsMakeNoShapes`,
  `TestProgramRerunsLetGo` and `TestJITProgramRerunsKeepLayouts` pin it. A
  literal that names its keys goes further, in any code: a realm keeps a root
  for those keys, and new code makes none of its shapes
  (`zzz_literalkeys.go`), past a quarter of the tree starting again
  (`TestFreshCodeMakesNoLiteralShapes`, `TestLiteralKeysMadeUp`). What remains:
  - Shapes are given back by a cleanup, after a collection. A host with a heap
    so large that it allocates 65,536 shapes between two collections, running
    new code for each request, finds the tree full until the next one, as
    prototypes made and dropped always could.
  - One object kept of a run holds the budget of the sixteen roots its root
    shares one with; one kept of a `Program`'s roots let go holds the whole
    program's, up to a quarter of the tree.
  - A realm's roots for literals' keys stay for as long as the realm, up to a
    quarter of the tree, what grew from them with them.

### KI-68 Code run once stays for what it left in a realm's tables
- **Where:** `internal/vm/vm.go` (`templateObject`), `vm_shape.go` (`chainCell`)
  and `zsourcemap.go`.
- **Effect:** a host that runs new code for each request -- `Eval`, a `Program`
  of its own, a direct eval or `new Function` in a loop -- grew until it closed
  the runtime, in what a table kept of each request's code:
  - A realm's template objects were by the function, held strongly, so that
    code with a tagged template was never collected, nor its script and
    source: 3 KB a run for a line of code, 61 MB in 20,000 runs, and 22,000 of
    22,000 functions alive.
  - A runtime with a source map loader kept each script that named a map and
    wrote a stack, with the map: 500 B a run with a map that failed to load,
    and all of a parsed map with one that loaded.
  - An object's list of the prototypes whose chains go through it was cut down
    only when the object changed, which `Object.prototype` does not: 25 B for
    each class new code made and read through.
- **Status:** fixed. A site's template object is the same for as long as its
  code can run, as every closure holds its function, and goes with it: the
  realm's table is by the function's address and holds it weakly
  (`templateSites`), so that a site evaluated again pays a lookup and a weak
  read, 7 ns a site more, and no `weak.Make`. The source maps' table holds its
  scripts weakly, and both tables and the list are looked through as they
  double, so that what is dead is dropped for what is added. So 20,000 runs of
  each add nothing to the heap. `TestTemplatesLeaveWithTheirCode`,
  `TestTemplateObjectsLastAsLongAsTheirCode`, `TestTemplateObjectsByRealm`,
  `TestTemplateEntryOfACollectedFunction`, `TestProtoUsersOfGoneCode` and
  `TestSourceMapsLeaveWithTheirScripts` pin it. What remains:
  - A private name, as a symbol used as a key, has an atom for the runtime's
    life (KI-07): a class with `#p` evaluated for each request costs 117 B
    each, whatever made it.
  - Each `delete` of a global leaves a tombstone in the global object's table,
    29 B, which stays: a site of a global keeps its slot for good, and a name
    added again goes last, as the standard's order has it.
  - A module stays in its realm's module map for as long as the realm, as the
    standard has it, so `EvalModule` with a new name for each request grows, by
    3 KB for a line.
  - A debugger lists every script it has seen, for as long as the runtime.

### KI-77 A substring is counted without the string it holds
- **Where:** `internal/vm/memory.go`, `string`, and what cuts a string out of
  another: `String.Substring`, `JSON.parse`, whose strings with no escape are
  slices of the document, and eval, whose strings and names are slices of its
  source.
- **Effect:** the walk counts a string at its own length, and an ASCII string
  cut out of another shares its bytes and keeps all of them alive. Sixty 11 MB
  strings each cut down to two characters, under a 32 MB limit, leave 715 MB
  of live heap and no error. Node's sliced strings hold their parents too,
  but are counted with them and are not made below 13 characters.
- **Status:** fixed, for a runtime with a memory limit; any other cuts as it
  did. A cut shares its parent's bytes only if it is at least half the size of
  its root, the string whose bytes they are, and a shorter one is copied, then
  counts exactly and is a root itself (`keptCut`, `zzz_strcut.go`).
  - The bound: a shared cut holds at most twice what it is counted at, a
    factor of 2 stated against the root's length; a root a builder made
    holds the slack of its buffer past that, as it did. A cut and its parent
    both live are counted apart, as they were, so that a script holding many
    long cuts of one string is stopped early, not late.
  - A shared cut records its root's size, rounded up to a power of two, as a
    log2 in `String.ropes`, so that a chain of halvings cannot creep past the
    rule. The byte is free there: `strAppend` reads it only on a string it made,
    with `building` set, which no cut has. There is no room for a pointer to
    the parent, as V8 has: `aux` can hold a shared slice's code units.
  - `s = s.slice(1)` stays linear: it shares until it is below half, copies
    once and shares again, and copies at most twice the string in all.
  - KI-77's sixty strings cut to two characters now leave nothing live, so
    that there is no error to give; cuts past half of each share, are
    counted, and end in `ErrMemoryLimit`.
  - `JSON.parse` under a limit keeps to the same rule, and gives every string
    its own allocation: a string of a batch held the rest of the batch (4 KB
    of text and 32 Strings) at no count. So does a regexp's match: a String
    in the room of a match array held the array and its input.
  - Other ways a string came to hold more than it said: eval's and Function's
    constants, names, templates and regexps were slices of their source, and
    outlived the code (`ownCode`); `Function.prototype.toString`, a reviver's
    `context.source`; and text that came back from Go with no mark, a Symbol's
    description, a RegExp's source, `normalize` of ASCII (`ownText`). The
    walk's own lists held what it last saw (`TestMemoryWalkKeepsNothing`).
  - Native code makes no cut: its substring, slice, substr and charAt make a
    one-character string of the runtime's or call the built-in, and its leaves
    and native regexps are refused under a limit.
  - Tests: `TestStringCutsUnderALimit`, of every built-in that returns a cut,
    `TestStringCutChains`, `TestStringCutBound`, `TestStringCutLoopStaysLinear`,
    `TestStringCutsOfKI77`, `TestStringCutsOfAProgramRunAgain`,
    `TestStringCutMarkIsNoAppendCount`, `TestStringCutLeavesItsRoom`,
    `TestJITCutsUnderALimit`.
  - What remains: live code is not counted, so that a function made by eval or
    Function holds its source at no count, as it did (KI-62's rule for what is
    the program's). A host function that cuts a Go string it was given and
    returns the piece holds the whole, as anything a host holds does.

## P2: wrong results

### KI-29 Generator and async frames skip part of a call's setup
- **Where:** `internal/vm/generator.go`. The resumed frame lacks `evalVars`,
  `thisRef` and `newTarget`.
- **Effect:**
  - A direct `eval("var x")` in a sloppy generator or async function leaks
    `x` onto the global object.
  - In a derived constructor, an async arrow sees `this` before `super()`,
    fails its own `super()`, and reads `new.target` as `undefined`.
- **Status:** fixed. A generator is set up as `run` sets up a frame, and each
  resumption restores it.

### KI-30 A generator resumed from another realm runs in that realm
- **Where:** `internal/vm/generator.go`. `resumeFull` never switches
  `r.Realm`.
- **Effect:** the generator's own realm's top-level `let` bindings become
  unreachable.
- **Status:** fixed. A resumption runs in the generator's closure's realm.

### KI-31 A non-writable array `length` can be bypassed
- **Where:** `internal/vm/vm_property.go` (`createOwnProp`).
- **Repro:** with `length` made non-writable, `a[5000] = 4` still extends
  the array.
- **Status:** fixed with KI-32.

### KI-32 `Object.preventExtensions` leaves dense arrays growable
- **Effect:**
  - Holes can be filled.
  - `length` can be raised and written past.
  - `arguments` can gain keys.
- **Status:** fixed. `createOwnProp` adds an element only to an extensible
  object, and to an array only below a read-only length.

### KI-33 Proxy invariants are checked against raw properties
- **Where:** `internal/vm/builtins_proxy.go`. The `has` and `ownKeys`
  checks read the target's property table.
- **Effect:** they miss an array's `length`, String indices, a lazy
  `prototype`, dense elements, and a target that is itself a proxy.
- **Status:** fixed. Both ask the target through its `[[GetOwnProperty]]`,
  `[[OwnPropertyKeys]]` and `[[IsExtensible]]`, in the standard's order.

### KI-34 Proxy trap errors are swallowed
- **Where:** `hasProp` on a proxy's target, and the `with`-scope lookups in
  `vm.go`.
- **Repro:** `"x" in new Proxy(new Proxy({}, {has() { throw 1 }}), {})`
  answers `false`.
- **Status:** fixed. The proxy half was fixed with KI-04; the `with`-scope
  lookups already passed errors on. What swallowed them was the global
  object's: a read, `typeof` and a strict assignment of a global name now ask a
  proxy on its chain `has` first, as V8 does, and throw what it throws. V8
  asks nothing before a strict assignment, which `WithNodeQuirks` follows.

### KI-35 Own keys list array indices out of order
- **Where:** `internal/vm/vm_object.go`. An index that became an accessor is
  listed after the dense elements.
- **Effect:** `Reflect.ownKeys`, `for-in`, `Object.assign` and structured
  clone read in the wrong order.
- **Status:** fixed. The table's indices are merged among the dense ones.

### KI-36 `for-of`'s array fast path diverges
- **Where:** `internal/vm/iterate.go`.
- **Effect:**
  - Iteration stops at the end of the dense elements, so freezing the array
    or growing it sparsely ends it early.
  - A replaced `%ArrayIteratorPrototype%.next` is ignored.
- **Status:** fixed. The walk goes to the array's length, reading what is
  not in dense storage with a Get, and every array fast path -- for-of,
  spread, destructuring -- steps aside once `next` is replaced.

### KI-37 Views don't see another runtime's growth of a shared buffer
- **Where:** `typedArrayData.count()` and `DataView` storage, and the
  constructors, read the buffer's cached length.
- **Status:** fixed. Both kinds of view, and both constructors, bring the
  length up to date before they measure it.

### KI-38 A `grow` that loses a race to a larger one succeeds *(plausible)*
- **Where:** `internal/vm/builtins_atomics.go`. The length check is made
  outside the memory's lock.
- **Status:** fixed. `grow` checks again under the lock and refuses a shrink.

### KI-39 Regular expression semantics
- **Counted quantifiers:** they never apply the empty-iteration check:
  `/(a|){0,2}b/.exec("ab")` gives `""` for the group, where the answer is
  `"a"`.
- **`/u` matching:** sticky and global matches can start in the middle of a
  surrogate pair.
- **Status:** fixed. A counted quantifier's iterations past its minimum are
  guarded as `*`'s are, and a `/u` or `/v` lastIndex between the halves of a
  pair reads from the first. 408 exec results agree with Node.

### KI-40 `Map` and `Set` keep `-0` as a key
- **Where:** `internal/vm/builtins_map.go`. `set` doesn't normalise the key.
- **Status:** fixed. A key, and a Set's member, of -0 is kept as +0.

### KI-41 `Set.prototype.intersection` works on a snapshot
- **Effect:** changes made by the argument's `has` or `keys` during the call
  are not seen.
- **Status:** fixed. It walks the receiver live, as `forEach` does, and steps
  the argument's keys one at a time.

### KI-42 Number formatting and parsing
- `(-0).toExponential()` gives `"-0e+0"`.
- `parseInt("0x")` gives `0`, not `NaN`.
- `Number("0x…")` and binary or octal strings are rounded twice, as is
  `parseInt` of a long decimal.
- **Status:** fixed. Radix 10 and powers of two are rounded once, correctly,
  and other radices take V8's approximation; 22415 seeded answers agree with
  Node.

### KI-43 `Math` inaccuracies
- `Math.log` of a subnormal is wrong.
- `Math.cosh(710)` and `Math.sinh(710)` give `Infinity`.
- `10**308 !== 1e308`.
- `Math.sumPrecise` loses `5e-324` beside `±1e308`.
- **Status:** fixed. Math's functions are a port of V8 14.6's fdlibm
  (`internal/fdlibm`), and agree with Node to the last bit on 66,000 seeded
  arguments; Go's differed on up to 900 of 3000 per function. `pow` and `**`
  are Arm's, ported from Arm's Optimized Routines under its MIT license:
  glibc ships the same code, which V8 calls on Linux. Node on Windows calls the C
  library's and rounds a few arguments in a thousand near a midpoint the
  other way. `sumPrecise` keeps all 2098 bits a double can span.

### KI-44 Code units counted as bytes or runes
- **`setFromHex`:** its odd-length check counts UTF-8 bytes.
- **`padStart` and `padEnd`:** they count the filler in runes, so a lone
  surrogate filler makes the result short.
- **Status:** fixed. Both count code units, and padding and `repeat` join
  the halves of a pair where copies meet; the hex errors take V8's message.

### KI-45 `Iterator.prototype.take` and `drop` reject counts above 2^53 - 1
- **Status:** resolved as a Node quirk. The standard (and test262) refuses a
  finite count past 2^53 - 1, as the engine did; V8 takes it. `WithNodeQuirks`
  now takes it too. Both modes give V8's message for NaN and negatives.

### KI-46 `toLocaleLowerCase` for tr, az and lt loses the final-sigma rule
- **Status:** fixed. Their loops ask `finalSigma` with the whole string.

### KI-47 Structured clone gaps
- The stdlib's own objects, `Blob` and `URL`, clone silently as `{}`.
- A port whose prototype was changed to `Object.prototype` or null is never
  shown to the host codec.
- An out-of-bounds view is cloned, not refused.
- Enumerability is checked again for each key.
- Two refusal messages differ from V8's: a proxy of a function, and
  `Object(Symbol())`.
- **Status:** fixed. The standard library brands each object it makes with
  how it clones, as Node's do -- unsupported, opaque, a Blob, or needing
  transfer -- in the object's internal slot, which a changed prototype does
  not hide. Out-of-bounds views are refused, enumerability is settled once,
  and the messages are V8's. `CustomEvent`, which the library lacks, is the
  one class Node has that this does not cover.

### KI-48 Messaging semantics
- A listener that throws stops the rest of the dispatch.
- `onmessage` runs after every listener, whenever it was set.
- `BroadcastChannel.postMessage` serializes once per receiver.
- A port in transit when its peer closes never hears `close`.
- `postMessage` on a closed port skips serialization, so it neither throws
  nor detaches.
- `deliver` calls a `dispatchEvent` the script can replace.
- A primitive in a transfer list, and bad `ports` for `MessageEvent`, are
  reported unlike Node.
- **Status:** fixed, as Node has each. A listener's exception is reported
  after the dispatch, as the standard library's `queueMicrotask`'s now are
  (they were dropped); the engine's own `queueMicrotask` hands what its
  callback throws to `OnUncaughtException`, or drops it without one.
  A port in transit already heard its peer close, since KI-23. Node starts
  a port on `addEventListener("message")`, where the HTML standard does
  not; this engine keeps the standard's.

### KI-49 Workers diverge from Node
- `process.on('unhandledRejection')` is overridden.
- A web worker ends on its first uncaught error; the HTML spec reports the
  error and keeps the worker running.
- `close()` cuts the running task short.
- A syntax error is reported as an `Error`.
- `process.exitCode` is ignored.
- **Status:** fixed. `Process.Unhandled` is called only when the script does
  not listen for `unhandledRejection`, in a worker and in qjs; `ExitCode`
  reads `process.exitCode`, which both end with; a compile error is a
  `SyntaxError`; a web worker reports an exception and runs on; and
  `close()` lets the running task finish, since KI-20.

### KI-50 qjs `file:` and `data:` URLs
- `file://LOCALHOST/`, `file://127.0.0.1/`, `file:///C:` and `file:C:/x`
  resolve wrongly, and an encoded `/` is accepted.
- A `data:` URL's `;BASE64` in upper case, unpadded base64 and a stray `%`
  are refused.
- *Plausible:* qjs's own error output doesn't take the lock the workers'
  writes take.
- **Status:** fixed. File URLs are read as node's `fileURLToPath` reads them
  on the platform, data URLs as the Fetch standard reads them, and qjs's own
  output takes a lock the workers' writes take too.

### KI-51 The `Intl` getter can build a second `Intl`
- **Effect:** calling the lazy getter again rebuilds `Intl`, which breaks
  formatters made from the first.
- **Status:** fixed with KI-13. `Intl` is built once.

### KI-52 Temporal constructors take the wrong realm's prototype
- **When:** `new.target.prototype` is not an object.
- **Status:** fixed. Temporal's prototypes are named intrinsics, as Intl's
  are, so another realm's counterpart is found.

### KI-53 BigInts above about 1.8e308 are formatted as `∞`
- **Where:** go-intl has no exact decimal for a BigInt.
- **Status:** fixed. go-intl 781eb48's `ParseExactDecimal` reads one without
  rounding it to a double's range, which `NumberFormat` and
  `BigInt.prototype.toLocaleString` use; a numeric string that large is
  still `∞`, as ECMA-402 has it.

### KI-54 `unwrapFormatter` reads the legacy symbol unconditionally
- **Effect:** there is no `OrdinaryHasInstance` check first, and it also
  unwraps where the spec does not.
- **Status:** fixed. Only the format getters and `resolvedOptions` unwrap,
  after `OrdinaryHasInstance`, and a built-in constructor's name is right in
  V8's receiver messages.

### KI-55 Out-of-range Temporal plain values are formatted
- **Expected:** a `RangeError`.
- **Status:** resolved as a Node quirk. The standard, as test262's
  `temporal-objects-no-time-clip` tests have it, formats them, as the engine
  did; V8 refuses a plain date whose midnight, or a plain date-time, is past
  the instants a Date can hold. `WithNodeQuirks` now refuses them too.

### KI-64 Native code of one realm ran in another
- **Where:** `internal/vm/zzjit_native.go`. The JIT's cache was keyed by the
  bytecode function alone.
- **Effect:** a compiled program run in two realms `WithJIT` ran the code of
  the realm that made it hot first. Objects, arrays, functions, arrows,
  regular expressions and inlined callees' objects came with that realm's
  prototypes: `Object.getPrototypeOf({}) === Object.prototype` was false.
- **Status:** fixed. Code is keyed by function and realm, as V8 keeps
  optimized code by native context, and a realm compiles its own.
  - Test: `TestJITRealmsHaveTheirOwnCode`, `TestJITCallsAcrossRealms`.

### KI-69 A function called through `.call` got itself as its `this`
- **Where:** `internal/jit/mir/amd64.go` and `arm64.go`, the native call's
  `setup`. A callee that reads its arguments past its parameters
  (`ssa.CallSite`'s `FrameArgs`) is not given its operands straight, and the
  receiver was read from the slot under the call's callee, where a method
  call has it. Through `Function.prototype.call` that slot holds the function;
  the receiver is the first argument.
- **Effect:** with the JIT, `G.call(recv, ...args)` of a `G` that reads
  `this` and `arguments` ran with `this` being `G`, with no error of its own.
  Once the loop below ran natively it gave `function0`, not `number0`:

  ```js
  function strict() { 'use strict'; return typeof this + arguments.length }
  function run(n) { let s; for (let i = 0; i < n; i++) s = strict.call(i); return s }
  ```

  A `this instanceof G` in `G` was false, and a sum over `this.k` went from
  73350 to 73255. A `G` that does not read `this` had none, so a stack frame
  said `named`, not `Shape.named`. A `G` that uses `arguments` is called so
  only if it has native code when the call site is decided, which happens
  once (`jitLinkable`), so a program could meet the bug in one run and not the
  next.
- **Status:** fixed. The receiver is read from the slot the call already
  worked out, the first argument for a call through `.call`.
  - Test: `TestJITCallThroughCallFrameArguments`, and
    `TestJITApplyFrameArguments` for `.apply`, which takes another path and
    never had the bug.

### KI-70 A nested loop stopped at a continue before a later test
- **Where:** `internal/vm/ztree_flow.go`, `flow.loopShape`. A loop whose body
  is tests runs as one Go loop, each test going on or leaving the loop. A
  test that went back to the loop's head by itself, where it went on to a
  later test by another way, was taken for a way out. `if (a) continue;
  if (b) break;` is that shape in a `while`, a `for...in` and a `for...of`,
  where `continue` goes to the head; a `for` loop's goes to its update.
- **Effect:** in the tree tier, with no error of its own, a loop nested in
  another stopped at its first `continue`, and the code after it ran as if it
  had finished: the outer loop has one thing to go on to from it, and asks for
  no result. A loop alone in its function went on from `runTree`, and where the
  whole function was one structure, whose blocks are dropped, it gave back
  `undefined` or a number left on the stack, or panicked with an internal
  error. This gave `e,e`, where Node gives `2,3,e,2,3,e`:

  ```js
  function f(o) {
    const log = []
    for (let i = 0; i < 2; i++) {
      for (const y of o) { if (y === 1) continue; if (y === 9) break; log.push(y) }
      log.push('e')
    }
    return log.join()
  }
  f([1, 2, 3])
  ```

  Native code was right, so a program's answer could change as it warmed.
- **Status:** fixed. `loopShape` leaves such a body to the blocks, which go
  from one to the next as ever. Found by `TestStrAppendRandom`.
  - Test: `TestLoopContinueBeforeALaterTest`.

### KI-71 A Go call in native code was given what had taken a box's register
- **Where:** `internal/jit/mir/core.go`, `allocate`. A box's word need not be
  kept: about fifteen places in each backend make it again of what it boxes
  (`remat`). What keeps that operand alive to them is a frame state naming the
  box, and a captured local's value is in no state, its slot being its cell.
- **Effect:** with the JIT, a number just stored in a captured binding reached
  a call of Go as whatever had taken the operand's register: `'' + s` gave
  `"0"` and `"1"`, a loop counter's low bit, for `NaN`, with no error of its
  own. A meter sends that call to an exit, which reads the cell, so
  `jitDifferential`, whose runtimes have one, could not see it.
- **Status:** fixed. A use of a box is a use of its operand, in `allocate` and
  in the check of its intervals. Found by `TestJITStrAppendRandom`.
  - Test: `TestJITBoxOperandAtGoCall`.

### KI-72 A template's part that was an addition stayed a number
- **Where:** `internal/jit/compile/lower.go`, `stringConcats`. A template's
  part is converted to a string (a concat of one), which is dropped for what
  is one already. Every `ir.StringAdd` was taken for that, but an addition
  that has met a string once becomes one by that alone (`hintedStringAdds`),
  and may be given two numbers afterwards.
- **Effect:** with the JIT, a template's value was the number itself:
  `` function f(a, b) { return `${a + b}` } `` called with strings until it
  was compiled, and then `f(1, 2)` gave the number 3, whose `typeof` was
  `number` and whose `.length` was `undefined`.
- **Status:** fixed. A conversion is dropped only for what cannot be anything
  but a string, an addition with such a string on a side; what is only taken
  for one still decides who makes an addition, where being wrong costs nothing.
  Found by `TestJITStrAppendRandomMarking`.
  - Test: `TestJITTemplateOfAnAddition`.

### KI-73 A native caller Go resumed read another local's object
- **Where:** `internal/vm/zzjit_ssa.go`, `jitUnwindNative`, and
  `zzjit_genstep.go`, `jitGenFrame`. A value native code took from a frame
  slot reads its pointer word through the slot's address for as long as it
  lives, whatever the local has been assigned since, because native code
  writes its frame only at an exit. A native caller whose callee left native
  code had the records of its call applied to its frame, for what reads the
  frame while Go finishes the call, and then went on natively after the call.
- **Effect:** with the JIT, plain JavaScript computed with the wrong object,
  with no error of its own, two native calls deep. This gave 22 for 12 once
  the call in `swap` left native code; the first run of the loop below gave
  240010 for 240000:

  ```js
  var obj = {a: 1}, A = {v: 1}, B = {v: 2}
  function inner(x, k) { const o = x === k ? 1 : obj; o.a = 1; return 0 }
  function swap(a, b, i, k) { var t = a; a = b; b = t; inner(i, k); return t.v * 10 + a.v }
  function run(n, k) { let s = 0; for (let i = 0; i < n; i++) s += swap(A, B, i, k); return s }
  ```

  Both functions must stay calls, not be inlined. Under `WithJITBackground`,
  which `qjs --jit` uses, more calls stay calls: Octane's Box2D stopped with
  `TypeError: cannot read property of undefined`, `t=e;e=f;f=r` in
  `CollidePolygons` taking a transform for a polygon. The run's own level was
  not affected: its records are applied only if it does not go on. Found in
  v0.34.2.
- **Status:** fixed. The words the records write over are kept and given back
  as the level goes on natively (`jitKeepSlots`, `jitRestoreSlots`), which also
  keeps alive objects that slot may have been the only reference to.
  - Test: `TestJITLevelResumesWithItsSlots`,
    `TestJITLevelKeepsWhatItsCalleeAssigned`.

### KI-74 A compile in the background taken for a refusal
- **Where:** `internal/vm/zzjit_ssa.go`, `jitCallSeen`, and `zzjit_native.go`,
  `tryJITAt`. Each asks a compile for code, which on the spot it has on return.
  In the background the same call returns nothing yet, which both took for
  no. A call whose target was an arrow reading `this`, which no empty link may
  take, was left to Go for good, and so was any such call while the runtime
  had `jitJobsMax` compiles in the background. A function compiled once hot
  had its closure refused, and no call entered the code it got.
- **Effect:** under `WithJITBackground`, which `qjs --jit` uses, right answers
  taking longer, by how the compiles fell. AWFY's Richards made 39 million
  calls of Go, at five times its time, in some runs and not others; Towers'
  recursion stayed in the tree tier, a first run of 700 ms for 100. AWFY as a
  whole took 8.4 s for 5.0 s in the foreground.
- **Status:** fixed. The call waits and is decided once its target has code
  (`jitCalleeAwaited`), and a closure is refused only for a refusal. AWFY in
  the background takes 5.2 s.
  - Test: `TestJITBackgroundCallWaitsForItsTarget`,
    `TestJITBackgroundHotCodeIsEntered`.

### KI-75 JSON.parse left a surrogate pair split between an escape and text
- **Where:** `internal/vm/builtins_json.go`, `parseEscaped`. A string with an
  escape is built up from its pieces, and text and escapes went in as they
  came. Two escapes that make a pair were joined; an escape and text that
  make one, a high surrogate escaped before a low one written as it is or the
  other way round, were not. A document can only have such text if a script
  put a lone surrogate into it, by concatenation, say.
- **Effect:** a string with the right code units and two spellings of its
  character, three bytes each where WTF-8 has one way to write it, which
  compared unequal to the same string made any other way. As a key it was a
  property no read of the pair found. Node joins them.
- **Status:** fixed. An escape of a low surrogate and text that begins with
  one go in by `wtf8.AppendJoin`, as a builder's pieces do.
  - Test: `TestJSONParseJoinsSurrogatePairs`, `TestJSONDifferential`.

## P3: small divergences

### KI-56 Intl details
- `DurationFormat` reads fields in the wrong order.
- `Collator.prototype.compare` is a new function each time.
- A compound unit gets "Internal error" instead of V8's message.
- Numeric strings are trimmed of U+0085. *Fixed with KI-10.*
- Lone surrogates compare equal.
- `supportedLocalesOf(…, null)`'s message differs.
- `new Intl.Locale("en-u-fw").firstDayOfWeek` is `""`.
- *Plausible:* `temporalTime` computes sub-milliseconds wrongly for a
  negative instant.
- **Status:** fixed, as V8 has each, but for `firstDayOfWeek`: test262 wants
  `""`, as the engine gave, and `WithNodeQuirks` now gives V8's `"true"`.
  Lone surrogates are weighed by code point in go-intl afe5892. A negative
  instant's sub-milliseconds were already right.

### KI-57 go-intl data
- `DisplayNames` `"weekOfYear"`.
- `"und-Zzzz-ZZ".maximize()`.
- `minimize()` with the `Zzzz` script.
- **Status:** fixed in go-intl v0.3.1.
  - `dayPeriod` and `timeZoneName` had no names either. All three are
    CLDR's `week`, `dayperiod` and `zone`.
  - `Zzzz` and `ZZ` count as missing subtags, as UTS #35 says.
  - `WithNodeQuirks` keeps ICU's answer, go-intl's `UnknownSubtags`: a
    locale that names all three subtags maximizes to itself. Standards mode
    gives `en-Zzzz-US` → `en-Latn-US`; Node gives `en-Zzzz-US`.
  - Test: `TestIntlUnknownSubtagsAndFieldNames`.

### KI-58 A cyclic `join` throws
- **Effect:** `a.push(a); a.join()` throws `RangeError`; V8 returns `"1,"`.
- **Note:** this is not a specification violation.
- **Status:** fixed, in both modes, as every engine does.
  - A join or `toLocaleString` (Array's or TypedArray's) of an object whose
    own join is under way already is `""`.
  - V8, SpiderMonkey and JavaScriptCore agree (Chrome, Node, Firefox,
    Safari, Bun).
  - The standard has no cycle check. `ToString` of an element that is an
    array calls `join` on it again, so a self-holding array ends only in
    stack exhaustion.
  - An element that joins its array once more differs from the standard:
    `"1,"` here and in every engine, `"1,1-x"` in the standard. No test262
    test covers it.
  - Test: `TestJoinCycle`.

### KI-59 The conformance agent harness hangs on a failing test
- **Effect:** an agent blocked in an infinite `Atomics.wait` ignores
  cancellation, and an agent's errors are dropped.
- **Status:** fixed.
  - Each agent's runtime is aborted when the pool stops, so its callbacks
    and waits stop with it.
  - An agent's first error is thrown to the main agent from `getReport` and
    `sleep`, and it fails the test.
  - Tests: `TestAgentPoolStopsAWait`, `TestAgentPoolReportsErrors`.

### KI-60 Host job details
- `notify` calls a host's `post` while holding the memory's lock.
- `HostJobsReady` keeps a stale signal after a loop is attached.
- **Status:** fixed.
  - `notify` tells asynchronous waiters once it has let go of the lock.
    This also removes a deadlock between a notify and a runtime closing.
  - `HostJobsReady` signals only while jobs wait. Attaching a loop and
    running the jobs both clear it.
  - `TestWaitForAges` had relied on the stale signal.
  - Tests: `TestNotifyDeliversUnlocked`, `TestHostJobsReadyAfterRun`.

### KI-61 A value doubled in a short loop escapes the memory limit
- **Where:** `internal/vm/memory.go`; the template literal's join in
  `vm.go`, `Array.prototype.join`, `JSON.stringify` and spread.
- **Effect:** with a 16 MB limit, `` s = `${s}${s}` `` or `s = [s, s].join("")`
  twenty times gave a string of 84 million characters, `a = [...a, ...a]`
  ran on until the process itself was out of memory, and
  `Array.from({length: 2e7})` was allowed. Each writes out as much as its
  inputs hold in one step, and the heap was only measured from the interrupt
  check, about once every thousand back edges: a loop of twenty turns never
  reached it.
- **Status:** fixed. What the engine makes at the script's bidding is
  charged as it is made, not only measured now and then:
  - a builder of script output -- the joins, `replace`, a template literal,
    `JSON.stringify`, a list read from an array-like, the values of a spread
    call -- charges what it holds as it writes, all of it, since nothing
    the walk can reach holds it yet;
  - what a built-in returns is charged at its size, and what a spread adds
    to an array, so that an output a few times its inputs cannot be
    repeated past the limit;
  - a charge that takes the estimate over the limit measures the heap, and
    a result the script already held is not counted twice.
  - Tests: `TestMemoryLimitDoubling`, `TestMemoryLimitResultsCountedOnce`.

### KI-62 The memory limit undercounts regular expressions and dates
- **Where:** `internal/vm/memory.go`. The walk counts only the engine's own
  types and skips those of other packages, which are shared or the host's:
  Intl's formatters, the compiled bytecode. A RegExp's compiled program is
  in `internal/regexp`, and so is not counted; part of a Date is held the
  same way.
- **Effect:** measured against Go's live heap after a full collection,
  10,000 regular expressions take 12.3 MB and are counted as 1.7 MB, and
  100,000 dates take 19.5 MB and are counted as 11.5 MB. A script that
  keeps making regular expressions under a 64 MB limit took the process's
  heap to 678 MB before it was stopped; objects or strings stop at 90-117 MB.
  Everything else is counted within 10%.
- **Status:** fixed.
  - The regexp package says what a pattern takes (`Regexp.Footprint`): its
    compiled program, which the RegExps made from one pattern share and the
    walk counts once, what each holds of its own -- its matcher, whose
    stacks a long match leaves large -- and the pattern it was cloned from.
    A class still in a shared Unicode property table does not count the
    table, which is the package's.
  - A Date's state is counted at its size; what it refers to is the time
    zone's, shared by every Date.
  - 10,000 regular expressions are now counted at 0.90 of their live heap,
    dates at 1.00, and a script making regular expressions under a 64 MB
    limit stops with a 121 MB heap, as objects and strings do.
  - Test: `TestMemoryMeterAccuracy`, which holds the estimate within 15% of
    the live heap for fifteen kinds of value. Before the fix, regular
    expressions were at 0.14, one pattern made many times at 0.70 and dates
    at 0.59.

### KI-63 Intl.Locale's newer test262 tests
- **Where:** `internal/vm/intllocale.go`, go-intl's `localeinfo.go`.
- **Effect:** test262's newer Intl Locale Info tests failed on a current
  checkout (macOS CI): `new Intl.Locale("en-u-co").collation` was `"true"`
  and `getCollations()` `["true"]`, where the standard has `""` and `[""]`,
  and so for `nu`; and `getTextInfo().direction` was `"ltr"` for scripts of
  no direction of their own (`Zyyy`, `Zinh`, `Brai`, `Zzzz`), unregistered
  or private use ones (`Aaaa`, `Qaaq`), and languages whose script cannot
  be found (`tlh`, `abcdefgh`), where the standard has `undefined`. Node
  v26.10 answers as the engine did.
- **Status:** fixed, as two go-intl divergences, `TrueKeywords` and
  `LeftToRightDirection`: the standards mode answers `""` for a keyword with
  no value, in every getter and list, and leaves a direction no one knows
  undefined; `WithNodeQuirks` answers `"true"`, ICU's `"yes"` in the lists,
  and `"ltr"`, as Node does. The `firstDayOfWeek` keyword of KI-56 now goes
  by the same divergence. Test: `TestIntlLocaleStandard`.

### KI-76 JSON.stringify read the space before the replacer
- **Where:** `internal/vm/builtins_json.go`, `stringify`.
- **Effect:** the standard reads the replacer, which for an array means its
  length and elements, and then the space, whose `valueOf` is the script's if
  it is a Number object. A proxy or a getter there and a `valueOf` saw each
  other's effects in the wrong order, and a revoked proxy as the replacer
  threw after the space's `valueOf` had run, not before. Node has the
  standard's order.
- **Status:** fixed. Test: `TestJSONStringifyReadsReplacerFirst`.

### KI-78 Symbol's own properties came in a different order each run
- **Where:** `internal/vm/builtins.go`, `initSymbolBuiltins`.
- **Effect:** the well-known symbols were defined by ranging over a Go map, so
  that `Object.getOwnPropertyNames(Symbol)` and `Reflect.ownKeys(Symbol)` gave
  another order in each process, and the atoms of those names, which the JIT
  compares a key with, had other numbers. Node's order is the same every time.
  `Symbol.for` and `Symbol.keyFor` came after them, too, where Node has them
  first.
- **Status:** fixed: Node's order. Test: `TestSymbolPropertyOrder`.
