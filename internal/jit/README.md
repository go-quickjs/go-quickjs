# Native JIT tier

This package is the optional native tier's machine-code side. The VM side
lives in `internal/vm/zzjit_*.go`.

The tier is **experimental and opt-in**, and it needs two things:

1. a build with `-tags quickjs_jit` on `linux/amd64`, `windows/amd64` or
   `darwin/arm64`;
2. a runtime made with `quickjs.WithJIT()` (or `qjs --jit`,
   `-conformance.jit`, `v8bench -jit`).

Without either, every function runs in the interpreter and the tree tier,
exactly as before (see [Zero cost when off](#zero-cost-when-off)).

This file states the design and the contracts the code keeps.

## Layout

| Package or file | Role |
|---|---|
| `ir` | The slot IR, one instruction per bytecode PC, with a state map at each. `Validate` checks a program, and `Evaluate`, a Go interpreter of the IR, is the oracle the SSA tests compare with. |
| `compile` | Lowers bytecode into the IR (`LowerSSA`, and `LowerSSAInline` for a callee to inline), or refuses it (`Refusal`). It never imports the VM. |
| `ssa` | Builds SSA from a program and the VM's feedback (`Feedback`), and optimizes it. `eval.go` runs SSA over a heap model, for tests. |
| `mir` | Selects instructions, allocates registers and encodes amd64 and arm64 machine code (`CompileAMD64`, `CompileARM64`), both on every target. |
| `abi` | The contract between the code and the VM: the context block (`Context`), the exits, and the value encoding (`Encoding`), given as data. |
| `asm` | The two assemblers. |
| `ssacode.go`, `arena.go`, `exit.go` | Placing code (`SSACode`, `Arena`), entering it, and reading its exits. |
| `barrier.go` | The runtime's write-barrier flag, which native code tests, and the Go side of its stores through the barrier (`storeRef`). |
| `verify` | A module of its own that checks the code `mir` makes for both targets against `golang.org/x/arch`'s disassemblers. |
| `entry_*.s` | Entry from Go (`enterSSA`), and the calls of Go native code makes (`callGo`). |
| `memory_*.go`, `cache_*.go`, `policy_darwin.go`, `jit_disabled.go` | Executable memory, the instruction cache, and the stubs for targets without the tier. |
| `internal/vm/zzjit_native.go` | The VM's side: selection, the code cache, budgets, host exits. |
| `internal/vm/zzjit_ssa.go` | Feedback for the builder, entering and resuming code, the calls of Go, native calls and their frames. |
| `internal/vm/zzjit_disabled.go` | The stubs for builds without the tier. |

The VM files are named `zzjit_` so that they sort after `ztree_*.go`.
Package vm's code is laid out in file-name order, so this placement moves
nothing the interpreter or the tree tier runs.

## Design

The tier follows V8's optimizing compilers where it can: it compiles what
the interpreter and the tree tier have seen run, speculates on what their
caches recorded, and deoptimizes when a speculation fails.

**Pipeline.** A hot function's bytecode is lowered to the slot IR
(`compile.LowerSSA`), one instruction per bytecode PC with the frame's
state at each. `ssa.Build` turns it into typed SSA (Braun et al.), asking
the VM's feedback (`ssa.Feedback`) at each site: a property's shape and
index from the site's inline cache, up to four shapes (polymorphic), the
prototypes a method was found on, a global's or a script lexical binding's
cell, the functions a call has called, a literal's pool. `ssa.Optimize`
unboxes numbers, merges guards a dominating one already made, removes dead
code and keeps references alive across stores and calls (`keep.go`).
`mir` allocates registers by linear scan and encodes amd64 or arm64; the
code is placed in the runtime's arena (`SSACode`).

**Speculation and deoptimization.** A guard checks what the code was
compiled for -- a number, a shape, a callee -- and exits to the interpreter
at the guard's bytecode PC if it fails. The failure is noted, and the code,
the function's current, is invalidated (`jitInvalidate`), as V8's
deoptimizer invalidates a function's code: Go enters it no more, native
callers no longer call it (its link is cleared), and the interpreter and
the tree tier run the function. Code that learned at its exits what it
is to be compiled again for -- a call's target at a call of Go, a
getter, an element's kind, a callee that inlines more -- is not
invalidated, as V8 never invalidates code for feedback: only it learns
those, which the existing tiers' caches do not note. It runs on, and is
compiled again in place once it has spent a budget as invalidated code
does, nothing more learned meanwhile (`jitLearnedDue`) -- a recursive
function's while its outer runs are still in its code, as V8 installs new
code while older frames are in the old: each such run, once what it
called returns, goes on from its frame, as the code it left describes it,
and in the new code at its next entry (only a native call of the code,
which returns into it, waits).

**Tier-up.** The budgets are V8's, named after its flags. A function is
given its entry, which holds its budget and its caches' mark as V8's
feedback vector does, after `invocation_count_for_feedback_allocation` (8)
calls (`jitInvocationsForFeedback`), and is compiled once the existing
tiers have spent `invocation_count_for_maglev` (400) invocations' worth
of it (`jitInvocationsForMaglev`; our one optimizing tier is the closest
to Maglev), each its bytecode's length (`jitTierBudget`): an invocation
spends its length, a full budget of back edges its count of them times
the loop's length (`jitTierWork`), as V8's JumpLoop spends its distance.
A loop still running when the budget is spent is entered by on-stack
replacement, as V8's `MaybeOptimizeFrame` does it, not after a fixed
number of back-edge budgets. Invalidated code waits for a fresh budget
the same way (`jitTierUp`), and code that learned at its exits too
(`jitLearnedDue`). Every invocation the existing tiers run spends it,
those the call path answers without a frame (a pure body's, a leaf's)
included when Go makes them for native code (`jitInvokedFrameless`), and
those Go runs for a native call of a function it does not enter itself
(`jitEnterCallee`): a function only native callers call is compiled again
by their calls. A
change of the function's caches' feedback -- what they hold, not how
often they were filled (`jitFeedbackMark`) -- raises what is left of the
budget to `minimum_invocations_after_ic_update` (500) invocations' worth
(`jitInvocationsAfterICUpdate`), never lowering it, as V8's
`NotifyICChanged` does; the change is found when the budget is spent.
Past `jitMaxDeopts` eager deoptimizations a function is compiled no more,
as V8 gives up optimizing one that keeps deoptimizing. `QJS_JIT_STRESS`'s
threshold compiles at once, as tests do (`jitCallThreshold` 1).

**Calls.** A call the VM has seen call a compiled function is made
natively: the callee's frame is made in the VM's stack, its context is the
next one, and its code is entered through a link (`jitLink`) that holds the
function's current code, so callers keep working when the callee is
compiled again or its code is dropped. As V8's callee builds its own
frame, the callee's code makes its frame and context at its call entry
(`mir`'s `Code.CallEntry`), once for all its callers: a call writes only
what it knows -- the arguments, the receiver -- and jumps there with where
it returns to, the callee's closure and its link in registers, and the
callee's return hands the result back in registers (`abi.Context`'s
`ReturnTo`). Small callees are inlined, with
their own frames made at an exit. A call is compiled for the functions it
was seen to call natively, up to `jitCallTargets` (4), each checked by its
object or, for a closure made anew, by its code (see Closures); one that
meets any other leaves native code, Go learning there what to call next.
A call that has met that many calls any other function through its link,
as V8 makes a megamorphic call through the code the function points to
(`ssa.CallSite`'s `Generic`): a closure of the caller's environment and
realm whose hint names its function's link (`abi.HintTable`), whose frame
is its arguments in its parameters (no rest, defaults, destructuring or
`arguments`), and not a generator's or an async function's. A callee
reached so, or by any native call, that has no code yet has its frame made
and leaves for Go to run it (`abi.ExitEnter`), which compiles it for
native callers. A call none of that covers -- a call of a built-in, of a
function the code does not call natively, a generic call's miss -- is a
call of Go (`ssa.GoCall`): native code calls into Go and goes on after it,
without leaving. As V8's deoptimization
data describes a call's frame, a call's state and a call of Go's operands
are static descriptions (`abi.CallState`, `abi.GoCallState`) of where the
code keeps each value -- a register its shared routine saved, a spill
slot, a constant, the frame -- which Go reads only when it needs them
(`jit.ExpandCall`, `jit.ExpandGoCall`): a site writes little more than the
description's address.

**Built-ins.** As V8 reduces calls of built-ins it knows, some are made
by the code itself, each call checking it calls the realm's function:
`Math`'s functions of numbers and `Math.random` (`ssa.Intrinsic`), a
string's `charCodeAt`, `String.fromCharCode`, a one-character `substring`,
`slice`, `substr` or `charAt`, `parseInt` of a number, an array's `push`
and `pop` on their fast paths, and `new Array()` or `new Array(n)` from a
pool. Strings' and arrays' `indexOf`, `lastIndexOf` and `includes` are a
call of Go of their own (`ssa.GoIndexOf`), which answers where the
built-in runs nothing. Anything else such a call meets is Go's.

**BigInts.** As V8's code makes its BigInt64 values, an operator of
BigInts the existing tiers saw held in their values (`abi.Encoding`'s
`ShortBigTag`) is made on them unboxed (`ssa/bigint.go`): as Int64s, or,
where the site met BigInts past the int64 range from 0 to 2^64-1 -- a
64-bit hash's or generator's -- as Uint64s. The arithmetic exits where a
result leaves its type's range, for a division by zero and for the most
negative int64 over -1; the bitwise operators exit nowhere; a shift is by a
constant. `BigInt(x)` of a number is made natively, and
`BigInt.asUintN(n, e)` and `asIntN`'s, of a width the call pushes as a
constant up to 64, check the function they call and cut `e`'s low bits;
where `e` is an operator that only such operators and the call take, it
is made on its operands' low 64 bits alone, as V8 truncates them to
machine words, its guards and the call's exiting to the state before the
first of them, which nothing since has changed. A loop's BigInts stay in
unboxed phis, and a box only frame states name is made nowhere but at an
exit, which makes the value of the unboxed BigInt (`abi.ExitLoc`'s
`IsBig`), as V8's deoptimizer materializes one. The rest -- BigInts on the
heap -- Go makes from native code (`ssa.GoArith`), giving back what the
tree tier would of its operands' blocks (`ssa.CallSite`'s `Release`). A
sum, a difference, a product, a quotient or a remainder is made first by
the VM's leaf (`abi.Encoding`'s `BigFast`, `jitBigLeaf`), in fewer steps
than Go's call: native code reaches it as it reaches `callGo`, through a
routine the Go runtime sees as `callGo`, a function its caller called; it
decodes the call itself, makes the result in a block given back, the
value before's, or a new one -- as V8's code allocates a BigInt inline --
gives back as the call would, and leaves what would throw to the call.

**Array loops.** A call of `Array.prototype`'s `forEach`, `reduce`,
`map`, `filter`, `some`, `every`, `find` or `findIndex`, or a typed array's
method of those names, is made a loop of the code's own, as V8's
JSCallReducer reduces it (`ssa.ForEachSite`): the array checked and its
length read once, then each element given to the callback, called
natively. `map`'s and `filter`'s arrays Go makes (`ssa.GoNewArray`, where
the array's species is `Array`), and the code appends to them. A hole, or
anything the loop's checks find, leaves, and Go goes on with the loop from
that index (`abi.Context`'s `Loop`), then native code after the call.

**Allocation.** Object, array and regular expression literals, closures
and their captured cells (see Closures), and the receivers of
constructions, are taken from pools of objects the VM makes ahead
(`abi.ObjectPool`), as V8's code allocates inline; Go refills a pool that
runs out.

**Exceptions.** The compiler emits structured code, so the handlers in
force at each instruction are known when lowering (`ir.Program`'s
`Handlers`), and native code registers none: wherever Go takes the frame
at an exit it gives it those handlers. A throw from what Go does there --
a call, an operation, a throw statement -- is unwound to its handler as
the interpreter unwinds it, and native code goes on at the handler, an
entry of its own, as V8's code goes on at a handler (`jitCatch`). A
finally clause's normal completion goes on natively; any other (a return,
a throw) deoptimizes, the interpreter reproducing it. A using declaration's
scope is such a try statement, its resources added and disposed of by Go.

**Iteration.** A for...of or for...in loop's cursor, and `yield*`'s
delegate, are Go's. Native code steps an array's for...of in place; any
other step is a call of Go, which answers the next value or, done, the
uninitialized value native code branches on. A step's iterator is closed
however the loop is left, as the interpreter closes it. `yield*`, an
`await using` scope's disposal steps, `import()` and `import.meta` leave
for Go and go on past it.

**Closures.** Code whose locals an inner closure captures keeps them in
cells of its own (`ir.Program`'s `Cells`), as V8 allocates a function's
context: the context's `Cells`, each an upvalue holding its value, which
native code reads and assigns through it, and frame states leave out. A
native callee takes them from its function's pool at its entry
(`ssa.GoCells`, mir's `cellsTake`); the frame Go entered the code with has
them open on its slots, as the interpreter's are. A closure is taken from
its template's pool (`closureTake`) -- in code with no cells too, where it
captures none of the code's locals -- and a scope's end gives its locals
fresh cells from the function's pool, holding what the old ones held,
which it closes (`ssa.GoClose`, `cellsClose`), as V8 allocates a block's
context at each iteration. Go makes them where a pool has run out,
refilling it, or a function has more than four cells. A cell native code
made on the frame Go entered is open on its slot again once native code
leaves the frame (the VM's `jitReopenRunCells`), as the cells of a native
callee's frame Go makes are (`jitReopenCells`). A call of a closure that
captures bindings checks the function the callee runs, in its environment
and realm, not the object (`ssa.CallSite`'s `ByCode`), and gives the
callee the closure it finds, as V8 checks a call's code: a callback made
anew at each call of its maker is a new object, the same function. Such a
closure is not inlined: an inlined callee's captured bindings are Go's.

**Generators and async functions.** `yield` and `await` are exits at which
Go suspends the frame as the interpreter does, and each resumption enters
native code at the frame's pc -- the entry after the yield or await, or,
the first time, past the parameters -- as the tree tier resumes a tree at
the block there (`jitResume`, `runGeneratorTree`). A call of one makes its
object, so none is called natively or inlined.

**Top-level code.** A script's, a module's or an indirect eval's code runs
once, so it is entered only at its loops, as V8 enters top-level code by
on-stack replacement: never at its start, and no refusal of it is kept in
the cache, which a loop evaluating thousands of scripts would fill. Its
declarations, which come before its loops, are left to the interpreter,
but for a `let`, `const` or `class` given its first value between two
loops, which Go gives it and native code goes on. A script's globals and
its top-level lexical bindings are read and assigned where they are, as a
function reads them (`ssa.OpGlobalCell`, `OpLexCell`). A module's bindings
are fixed once it is linked, so its code reads each one in the object that
has it, at that object's address (`ssa.OpBindingCell`), as V8 reads a
module's variables in its context and an import in its exporter's cell:
its own bindings in its environment, an import where the binding it names
is, found again from the modules already loaded, and a name the module
does not declare in the global object. An imported `const` is a constant.
A direct eval's code, which reads its caller's scopes, stays in the
interpreter.

**Tail calls.** A call in tail position gives up its frame for the
callee's only deep in the stack, as the interpreter's does
(`tailCallDepth`): a shallow one keeps its frame, as a stack trace shows,
and native code makes it as any call (`ssa.OpTailRoom`, `OpTailSelf`, by
`abi.Context`'s `TailLevel`). Deeper, a call of the closure the frame runs
writes its arguments to the frame's parameters and enters the code at its
start again, a loop in the frame, as a compiler turns tail recursion into
one; in a native callee's frame, a call of another function the call has
puts that function's frame in this one's place, its context this one's,
and goes on in its code, returning where this frame would have. A frame Go
entered the code with, which the VM's frame describes, is given up by Go
(`errTailCall`), as the interpreter gives it up. Frames the interpreter
keeps for such a call -- a construction's, an arrow's sharing a derived
constructor's this -- native code keeps too (`TailFrame`).

**Accessors.** A read whose site found a getter on its receivers'
prototypes, or a write a setter -- a class's accessor, or one
`Object.defineProperty` put on a prototype -- is a call of it, as V8's
code calls an accessor its feedback knows (`ssa.AccessorSite`): the
receiver is checked as a read found on a prototype is, the accessor in
the holder's cell checked to be the function compiled for
(`ssa.OpAccessorIs`, since a redefinition changes the pair in place), and
the getter is inlined, called natively through its link, or, where it
has no code, Go makes the read from native code (`abi.GoCallGet`,
`GoCallSet`), through the site's cache. The receiver is the getter's
this; a setter's result is dropped. Frames Go makes for such a call show
the accessor as the interpreter's do.

**Map checks.** A property read or write is compiled for the shapes its
site's inline cache has met -- the one it holds and those it held before
(`propCache.seen`), up to four (`jitPropertyCases`). A receiver of any
other shape, and any
receiver at a site whose cache knows none, deoptimizes (`ir.GuardExit`),
as V8's map checks do and its code does for insufficient feedback: the
interpreter makes the operation, the site's cache learning the shape --
the deoptimization fills it where the interpreter's read of a small table
would not -- and the code is compiled again for it (`jitPropertyDeopt`).
A read whose cache is empty, though the interpreter, which fills none
there, may have run it, searches its receiver's own small table, and
deoptimizes only where that has not the property (`PropertySite.Scan`).
What a cache cannot hold -- a primitive receiver, an object with no shape,
an object's own accessor, more shapes than the cases hold -- makes the
site megamorphic, once (`jitDeoptSeen`). Inside an inlined callee, whose
exits all leave their operation to Go, such an exit has the code that
inlined it compiled again so, and is not counted as the callee's leaving
(`jitShapeMiss`).

**Megamorphic reads.** Only at a site whose cache is megamorphic -- it gave
up (`maxCacheFills`), its cases are full, or it met what no case holds --
does a receiver of another shape have its own table searched and look in
the runtime's megamorphic cache (`abi.MegaCache`) before it leaves for Go,
as V8's megamorphic loads probe its stub cache: an entry, by the
receiver's shape and the key, names the receiver's own data property, or a
prototype's along a chain the code checks object by object and shape by
shape (`abi.MegaChain`), or that none of the prototypes has it, the read's
value undefined (`abi.MegaAbsent`). Go fills an entry at a read's exit. A
read whose receiver's prototypes are no longer its case's -- a method
added to one -- looks there too. A megamorphic site's write that neither
the receiver's own table nor the megamorphic cache has -- an addition, a
shape it never met -- is a call of Go (`ssa.GoCall`, `jitPropertyMade`) on
a branch the builder makes within the instruction (`splitOn`), the write
true where it stored nothing, native code going on where the two ways
meet, as V8's megamorphic stores call the runtime on a miss; the analyses
see the call on its branch alone. A read of an object there is so too,
its two ways each making a value (`split`): its cell none where it finds
no property (`OpCellNone`), the value loaded from the cell on one branch
and Go's on the other, which a phi takes, the call filling the
megamorphic cache as an exit would (`jitPolySeen`). In an inlined
callee, whose frame Go cannot make, either leaves.

**Private members.** A private member is a property under a key each
evaluation of its class mints, a symbol, which a binding the class's
methods capture holds. A read or a write of one is a property's (an
`ir.PropertyRead` or `PropertyWrite` marked `Private`), native in objects
of the shapes its site met, which the VM learns as Go makes it at an exit
(`jitPrivateSeen`), as V8's inline caches learn a private name's maps; and
`#x in o` is true natively where the read would find the member. Two
evaluations of a class share their methods' code, so the code checks that
the binding holds the key it was compiled for (`ssa.OpSameSymbol`). Go
makes the rest -- a member's definition, an accessor, a key in a local,
and a class's or an object literal's other members (`jitHost`).

**The in operator.** `k in o` (`ir.HasProperty`) is a call of Go from
native code (`ssa.GoHas`), as V8's code calls its HasProperty builtin: Go
searches the object and its chain as the interpreter does, where that runs
nothing and throws nothing. A proxy on the chain, whose has trap would run,
a module namespace, a key that is an object, whose conversion calls its
`toString` or `Symbol.toPrimitive`, and a right operand that is no object,
which throws, Go refuses: the code leaves for Go to make it there
(`jitHost`), and goes on after it.

**The delete operator.** `delete o[k]` and `delete o.k`
(`ir.DeleteProperty`) are a call of Go from native code (`ssa.GoDelete`,
`GoDeleteStrict` in strict code), as V8's code calls its DeleteProperty
builtin: Go deletes the object's own property as the interpreter does,
where that runs nothing and throws nothing -- an ordinary object's, its
shape changed as the interpreter's deletion changes it, an array's
element, which becomes a hole, a property absent. A proxy, whose
deleteProperty trap would run, a module namespace, a typed array, a
string wrapper, a mapped arguments object, a key that is an object, a
base that is no object, and in strict code a property that cannot be
deleted, which throws, Go refuses, having changed nothing: the code
leaves for Go to make it there, and goes on after it. The call clears the
deleted property's cell, so the analyses take it for a store of any key
(`ssa`'s allocOnly): a value read from that property and used after it is
kept first (`keepAcrossStores`), as across a call of a function; and every
property operation after it checks the object's shape again.

**Destructuring.** Destructuring from an iterator takes a cursor, as a
for...of does: Go makes it, called from native code (`abi.GoCallForOf`).
Each element is a step that goes on with undefined once the iterator is
done (`ir.IterStep`'s `When`, `abi.GoCallFill`), native over an array
walked in place and Go's otherwise, as a loop's step is. Its end closes the
iterator natively where there is nothing to close -- the cursor done, or
an array's -- and by Go where a return method must be called
(`ssa.OpIterClosed`); a for...of's break closes it so too. What is left,
for a rest element, is Go's.

**Profitability.** Each native stretch's work is estimated, exits and
calls of Go counted against it; code whose stretches do too little between
them is left to the tree tier (`jitSSAProfit`): Go's entering native code
and its leaving cost what V8's calls between its tiers do not, and a call
of Go costs more than the tree tier's own call of the built-in. A run
on-stack replacement began judges its calls of Go as it makes them
(`jitRunGoProfit`), not only when its stretch ends -- the end of the loop
-- and leaves after the call that finds them not to pay; and its
stretches are judged apart from those of entries at the start. Code
judged so (`loopSlow`) is entered neither at its start nor at a loop. The
frame it left goes back to the tree that entered it at a loop, once the
interpreter has run it back to that loop's header, the tree going on with
the loop as though it had not been entered (`errBackToTree`); any other
frame goes on in its tree from the next loop header (`runTreeFrom`), a
tree of handlers or for...of loops too. Code that
deoptimizes is invalidated instead (Tier-up).

### Reading the heap in place

Native code reads the VM's objects where they are, through the offsets
`abi.Encoding` gives, rather than through copies: an array's elements, a
property table, a binding's cell. A reference it reads keeps the address of
the cell it came from as its source, and Go reads the reference back from
there at an exit. Reading an address back as a pointer is outside what
`unsafe.Pointer`'s rules promise; it is sound while Go's heap does not move
and nothing else changes the object graph while native code runs, which a
`Runtime` (not safe for concurrent use) guarantees. It is checked with each
new Go: `TestJITObjectLayout` pins the fields native code reads, and the
JIT's tests run under `-d=checkptr=2` and `GODEBUG=gccheckmark=1`.

### The write barrier

Native code tests the runtime's write-barrier flag before it writes a
pointer, as compiled Go does. While the collector marks, it calls Go's
barrier, as V8's code calls its own: a store of a pointer word -- a
property's value, a property added along a transition and its shape, an
element, a keep -- jumps to `storeRef` (`entry_*.s`), which, as `callGo`,
is to the Go runtime a function the code's Go caller called, and calls
`storeRefGo`, a nosplit Go function that stores the word, so that the
compiler's barrier shades the pointer it overwrites and the one it writes.
A routine each code shares saves the registers the code allocates and
restores them after; `storeRef` keeps only the context's register. The
flag cannot change between the test and the write: Go sets it only with
the world stopped, which native code, never a preemption point, is not.
Emulating the barrier's buffer in native code was rejected: its contents
would have to reach the collector before any safepoint Go could stop the
goroutine at.

A native call is made natively whether the collector marks or not, with
no barrier in its setup: what it writes -- the caller's records, the
callee's context and its frame past the VM's stack's top, the callee's
locals made undefined -- each outlives none of what it is written from
(the caller's frame, keeps and records, which last until the call
returns), and overwrites only what nothing reads again, an earlier
call's. The one store that outlives its source is the result's, into the
caller's keep cell (and a `super()`'s into its `This`), from the
callee's frame, which the next call overwrites: while the collector
marks, its pointer word goes through `storeRef`, after a return or after
Go finished a callee that left native code (`RetValue`). So Go resumes
native callers, and they call on natively, whether the collector marks
or not. What moves a pointer into what outlives it does so through
`storeRef` while the collector marks: an object taken from a pool has its
pool's cell cleared through it, which shades the object, or, a
construction's receiver, is shaded in its pool before the call takes it;
what native code then writes into it -- an array literal's elements, a
closure's captured cells, the arguments in an entry's cells -- and a
value pushed or popped go through it too. A loop's step copies an
array's element with no barrier: the element stays in the array.

What native code holds where the collector does not look while it
rewrites what the value came from -- a call in tail position's operands,
read into `GoArgs` before the frame is written over -- it shades first,
storing each pointer through `storeRef` into the context's `Shade`, so
that a collector scanning the frame midway misses none. An exit's records
are the one thing native code leaves to Go while the collector marks
(`recordsTail`): read into `GoArgs` in one pass and written in a second,
they would need the same, at an exit, where Go is called anyway. Nothing
else is Go's for the marking's sake, and nothing leaves native code for
it: code is compiled only with `storeRef` (`abi.Encoding.StoreRef`).

## The execution boundary

**Entry.** `enterSSA` is `NOSPLIT|NOFRAME`. It tail-jumps into the code with
the context block in a register, and the code returns with a plain `RET` to
`enterSSA`'s Go caller. Native code:
- never moves SP or touches the frame pointer;
- never executes a call instruction: it calls another function's native
  code by jumping there, with that level's context, and Go by jumping to
  `callGo` (`abi.Context.GoOp`);
- uses Go's ABI0, not the platform C convention, on every OS.

**Calls of Go.** To the Go runtime, `callGo` is a function the Go function
that entered the code called: its frame and return address are all a
traceback, a stack scan or the stack's growth meets. Go may do anything
there, run JavaScript included, and the code goes on where Go says.

**Registers.**

| | amd64 | arm64 |
|---|---|---|
| Never written | SP, BP, R14 (g) | SP, FP (R29), LR (R30), R18 (platform), R28 (g) |
| Clobbered | the rest, X15 included | the rest, R19-R25 included |

ABI0 callers expect clobbering: on amd64, Go restores X15 and R14 after any
ABI0 call. `verify` checks these rules for every function its corpora
compile.

**Polls.** Every back edge decrements the interpreter's back-edge counter
(`Context.BackEdges`). When it runs out, the code calls Go (or, under a
stress budget, exits to it), which checks for cancellation, `Halt` and the
memory limit, gives the loop a new budget, and lets the code go on.

**Not a safe point.** Generated code is never a safe point. SIGURG, Windows'
`SuspendThread` and SIGPROF see a PC outside Go's text:
- preemption retries until Go code runs again;
- a profile sample counts as external code.

The back-edge budget, and the calls of Go, bound how long the goroutine can
delay a stop-the-world pause.

**Faults kill the process.** Go cannot recover a fault at a PC outside its
text:
- on Windows the exception goes unhandled;
- on Unix it is fatal, except a nil-page access, which `Runtime`'s recover
  turns into an error.

So a backend bug can take down the host. The defences are verification and
differential testing: the SSA evaluator and the IR oracle, the VM's tests
under every stress setting (`QJS_JIT_STRESS`), the differential fuzzer, and
`verify`.

**Compiler panics are refusals.** Lowering and the backend recover a panic
into a refusal, and the function runs in the existing tiers.

## Values and the garbage collector

The code uses the VM's frame and objects in place (see
[Reading the heap in place](#reading-the-heap-in-place)):
- It holds a slot's number word, never its pointer word. A reference stays
  in the slot that held it at entry, which native code does not write before
  it exits; an exit has Go copy it where it goes, and store a primitive over
  a reference (`abi.Record`). An ambiguous phi's shadow tells Go, at run
  time, which slot a value came from.
- It reads an object through the pointer word of the slot that holds it,
  at the offsets `abi.Encoding` gives: an array's class, flags, length
  and elements; a property table, by its shape; a script's global and
  lexical bindings (`Context.Global`, `Context.GlobalLex`).
- A store of a pointer tests the runtime's write-barrier flag first, as
  compiled Go does, and while the collector marks is made with Go's
  barrier (see [The write barrier](#the-write-barrier)).
- References it must keep across calls of Go are kept in the context
  (`Context.Keep`), where the collector sees them.
- It keeps no address between entries.

## Exits and deoptimization

**State maps.** Each reachable PC has a state map: the bytecode PC and the
live operand depth. An exit names one, and its records say what Go writes
into the frame.

**Exits.**
- A guard (`ExitDeopt`) fails before its instruction commits anything. Go
  writes the frame at that PC and the interpreter runs the rest of the
  invocation, entering code compiled again at a loop's back edge if its
  budget is spent meanwhile. The failure is noted, and the code is
  invalidated (see [Tier-up](#design)).
- A host exit (`ExitHost`) leaves one instruction to Go, which runs it, and
  the code goes on at the next entry.
- Code is entered only at a function's start, at its loops' headers, and
  after what Go is sure to run (an instruction left to it whole, a call),
  as V8's code is entered only at a function's start and by on-stack
  replacement at a loop. Where Go goes on at a PC with no entry -- after a
  guard's exit, or a frame native code called natively that Go finishes --
  the interpreter runs only to the next loop header, and native code from
  there (`jitGoOn`).
- A native callee that cannot go on leaves its frame to Go, which
  finishes the call in the interpreter, and its caller's code goes on
  after the call. One that has no code (`ExitEnter`) has only its frame
  made, and Go runs it from its start.
- An exit may leave its frame for Go to write from the exit's description
  and the registers it saved (`ExitTable`), Go then reading the exit's own
  kind.
- Work already committed, earlier loop iterations included, is never
  replayed.

## Selection and the code cache

**Function entry.** A closure counts framed calls. The eighth gives its
function an entry (`jitInvocationsForFeedback`), which compiles it once the
existing tiers have spent its budget (see [Tier-up](#design)).

**Loops.** A loop may also promote at a back-edge check: on-stack
replacement at the loop's header, from the interpreter or the tree tier.

**Native calls.** A call the VM has seen call a function it may compile is
made natively, or inlined. A callee whose code Go never enters on its own is
compiled for native callers only.

**The cache.** Each runtime keeps its own cache, weakly keyed by the shared
bytecode function. Shared bytecode carries no JIT state. It holds as many
functions' code as the JIT's budget has room for (see [Memory](#memory)):
no count of entries bounds it.

**Closure hints.** A closure remembers its code by a hint, a slot in the
cache's hint table (not a pointer), which holds the entry and its
function's address. The hint finds the entry only for a closure of that
function, so a stale hint misses however often its slot was reused. The
hint lives in what a template's closures share (`closureCode`), so the
closure stays 64 bytes; the table grows to the most entries the cache has
held at once, its free slots reused.

**Refusals.**
- Permanent: lowering or the backend refused it. These are cached, and the
  closure is marked refused.
- Deferred: the budget had no room for the code. These are cached until
  the cache releases code.
- Temporary (no budget at all): retried after another warm-up. When the OS
  denies executable memory, the runtime marks the JIT unavailable for good.

**Giving up.** Code whose native work per exit stays low is entered by Go no
more at its start (`entrySlow`, `jitSSAProfit`), and runs for its native
callers, and by on-stack replacement at a loop that has run a full budget
of back edges in the existing tiers: a long invocation, which pays -- but
not where the stretches such entries begin, or its calls of Go, do not pay
either (`loopSlow`), until Go retries it (`jitRetrySlow`).

**Debugger.** Runtimes made `WithDebugger` never run native code.

## Memory

**Allocation.**
- A runtime's code lives in its `Arena`: chunks of 64 KiB (a larger
  function gets a chunk of its own), each function placed after the last
  at 64-byte alignment. A runtime compiling 101 functions holds them in 2
  mappings (`TestJITArenaMappings`).
- Placing a function makes the pages it touches writable, copies it, and
  seals them read/execute again. Pages are never writable and executable at
  once, and published code is never patched. Other code in those pages does
  not run meanwhile: an arena is one runtime's, and its native code always
  returns to Go before Go compiles anything.
- A chunk is unmapped when the last function in it is released. The arena
  locks, since a finalizer can release code on another goroutine.
- Windows flushes the instruction cache with `FlushInstructionCache`.
- Darwin uses `ic ivau` with barriers.

**Darwin policy.** Darwin refuses executable memory to hardened or
code-signing-enforced processes (`ErrUnavailable`). There is no `MAP_JIT`
path yet.

**Not the script's memory.** The memory meter never counts the JIT, so a
script's measure, and whether it fails with `ErrMemoryLimit`, is the same with
the JIT on and off. The JIT has a budget of its own (`jitBudget`):
`WithJITMemoryLimit`'s, or 8 MB, or an eighth of the memory limit.

**The budget bounds all the JIT holds** (`jitState.held`), as V8 bounds its
code and what describes it by memory, not by a count of functions:

- each entry: its machine code as placed, its entries' offsets and its
  exits' descriptions (`SSACode.Footprint`), what it was compiled with and
  what it has learned, and its object pools with the objects made for them;
- the programs it lowered, one lowering a function (`ir.Program.Footprint`);
- the contexts native code runs in (4 at first, as many more each time they
  run out, up to 96: some 17 KB each), the hint table, the links, the
  compiles' workspaces between compiles, and code a suspended call of Go
  still holds.

Entries are measured at each compile and once an epoch; the rest as it
changes. Not counted: the arena's free space between live functions, which
it does not reuse until a chunk empties.

**Making room** (`makeRoom`). A compile asks for room for what its entry is
likely to hold (bytes per bytecode instruction, from the last compiles),
then, compiled, for exactly what its entry will hold before its code is
placed. The cache reclaims, in this order, each least recently used first:
lowerings of functions it has no entry for; refusals remembered; code not
used in the last two epochs (old, as V8 flushes code by age); the
workspaces. Code used within two epochs, code that runs, and an entry being
compiled are never reclaimed. If all it may reclaim would not make the room,
it reclaims nothing and the compile is refused: the function runs in the
existing tiers until some code is old. An epoch is a collector cycle, or
50 ms.

**Release.**
- `ReleaseClosed` releases code once native frames have unwound; code a
  suspended run still uses stays until the run leaves it.
- A failed release keeps ownership for a retry.
- A finalizer on `SSACode` reclaims what a host abandoned.

Runtime construction allocates nothing for the JIT.

## Zero cost when off

**Without the tag.**
- `jitBuilt` is false, and every hook in the interpreter and the tree tier is
  either an empty stub or inside `if jitBuilt`, so it is compiled out.
- `internal/cmd/hotdiff` checks the result. Compare an untagged `cmd/qjs` at
  main with one at the change: `executeAt`, `runTree`, `runTreeNested`,
  `callObject`, `callTree`, `runFD` and `backEdgeCheck` must be the same,
  instruction by instruction.

**With the tag, JIT off.**
- The JIT's fields fit existing padding: `closure` stays 64 bytes.
- `Runtime`'s and `Realm`'s JIT fields sit at the end of their structs, so no
  offset the interpreter reads moves. `TestJITFieldLayout` checks both builds.
- Measure the remaining flag tests with `placements`.

**With the JIT on.** Code the JIT never runs keeps the tree tier's fast nested
calls: only a frame whose loop may still promote keeps `runTree`'s recover
(`jitTreeRecovery`).

## Validation

```sh
go test ./internal/jit/... ./internal/vm                      # untagged
QUICKJS_REQUIRE_JIT=1 CGO_ENABLED=0 go test -tags quickjs_jit ./internal/jit/... ./internal/vm .
go test -tags quickjs_jit -race -count=1 ./internal/jit/...
go test -tags quickjs_jit -gcflags=all=-d=checkptr=2 ./internal/jit/... ./internal/vm
go test -tags quickjs_jit ./internal/jit/compile -run '^$' -fuzz '^FuzzLower$' -fuzztime=30s
(cd internal/jit/verify && go test -tags quickjs_jit ./...)   # encodings, both arches
TEST262_DIR=/path/to/test262 go test -tags quickjs_jit ./conformance \
  -run TestConformance -count=1 -timeout 60m -args -conformance.jit
```

- `QUICKJS_REQUIRE_JIT=1` fails a native test job that would otherwise pass
  by falling back.
- The VM's JIT tests force promotion and assert native entry, so they can't
  pass in the existing tiers.
- `-conformance.jit` keeps the normal thresholds, under which few test262
  tests reach native code. `QJS_JIT_STRESS=threshold,budget=3,deopt=7`
  compiles every function at its first call, polls every three back edges,
  and finishes every seventh poll's invocation in the interpreter.

`disasm -jit` reports a function's eligibility and IR without native support:

```sh
go run ./internal/cmd/disasm -jit -func sum -e \
  'function sum(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s }'
```
