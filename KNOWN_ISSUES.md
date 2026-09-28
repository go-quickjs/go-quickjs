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
    (`builtins_proxy.go`, `property.go`)
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
  one month at a time. Node hangs here too.
- **Status:** fixed, but for Temporal's Chinese `until`, which Node shares,
  and `segment()` splitting up front, which is linear.

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
- **Where:** `internal/vm/property.go` (`createOwnProp`).
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
- **Where:** `internal/vm/object.go`. An index that became an accessor is
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
  after the dispatch, as `queueMicrotask`'s now are (they were dropped).
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
  - Tests: `TestNotifyDeliversUnlocked`, `TestHostJobsReadyAfterAttach`.
