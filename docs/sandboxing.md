# Sandboxing

A runtime has no I/O, no network access, no timers, no filesystem and no module
loader unless the host adds them. There is no `require`, no `process`, no
`fetch`. `Date` reads the clock through an injectable hook rather than the
process clock. Everything in [the standard library](stdlib.md#the-standard-library) is
something a host hands over deliberately, and the [qjs](qjs.md#the-qjs-command)
command hands over only what its flags name.

`WithoutCodeGeneration()` removes `eval` and the `Function` constructor. Neither
grants a script a capability it does not already have — code it could `eval`, it
could also write inline — but both defeat review of the source a host is about
to run, which matters when that source is audited before use.

Bounds are set at construction:

```go
rt := quickjs.New(
    quickjs.WithMemoryLimit(64<<20),
    quickjs.WithStackSize(1<<16),
    quickjs.WithMaxCallDepth(1000),
)
```

A script that holds more than its memory limit is stopped, and the call
running it returns `quickjs.ErrMemoryLimit`, which the script cannot catch.
The runtime measures its own heap by walking it, as QuickJS does, once the
process has allocated a quarter of the limit since it last did. Between
walks, what the engine makes for the script is charged as it is made -- an
allocation whose size the script chose before it is made, a join, a
template, JSON text or a spread as it is written, a built-in's result once
it is made -- and a charge that would cross the limit measures the heap at
once. So a value doubled a few times over in a short loop is stopped as
surely as one grown a little at a time.

What the limit counts is what the script holds: the objects, strings, arrays
and buffers it can still reach, each counted once, at the sizes the engine
lays them out in. It is not what the script has allocated over its life --
a script that makes and drops far more than its limit runs to its end -- and
it is not what the process takes from the system:

- Garbage is not counted. Memory the script has let go of is free as far as
  the limit is concerned, though Go has not yet collected it.
- Go lets its heap grow past what is live before it collects. With Go's
  default settings, a script stopped at a 64 MB limit had taken the
  process's heap to 90-117 MB on the way. Size a host at about twice the
  limits of the runtimes it runs at once, plus what the runtimes' built-ins
  take, or bound the process as a whole with `GOMEMLIMIT`
  (`runtime/debug.SetMemoryLimit`), which makes Go collect sooner as it
  nears it.
- One process heap serves every runtime in it, so a limit is per runtime,
  not per process.

Measured against Go's live heap after a full collection, the estimate is
within 10% for objects, arrays, strings, Maps and Sets, typed arrays,
closures and BigInts. Two kinds of data are undercounted, which
[KI-62](known-issues.md#ki-62-the-memory-limit-undercounts-regular-expressions-and-dates)
records: a regular expression's compiled program is not counted at all, so
a script holding many regular expressions can hold several times its
limit, and a Date is counted at about 60% of its size.

So is the language a script means when it formats something without saying
which language to format it in -- what `Intl` answers with when it is given no
locale, and what `Date`'s `toString` and `toLocaleString` write in:

```go
rt := quickjs.New(quickjs.WithLocale("de-DE"))
rt.SetTimeZone(time.UTC)   // and the zone its local-time methods use
```

Standards-conforming behavior is the default. A host that needs exact Node.js
compatibility for known Node divergences can opt in with
`quickjs.WithNodeQuirks()`; the command-line equivalent is `--node-quirks`.
In `Intl` and Temporal each divergence is one of go-intl's named ones, from
the Japanese `h12` preference and the Islamic eras to the time zones
`Intl.supportedValuesOf` lists and two roundings of Temporal the standard
fixed after temporal_rs 0.2.3; go-intl's [compatibility
profile][go-intl-compat] lists them, with what the standard and Node each
answer. In the language it follows V8: strict code may assign to a call,
which is a `ReferenceError` when it runs rather than a `SyntaxError`, and a
function declared in a block is hoisted over the arguments object and over a
function an enclosing block declares with the same name, and a script's
functions and vars are created in the order they are written rather than the
functions first. Where the two agree, both modes answer as Node does.

Left unset, the runtime takes the language the machine is set to -- the user's
locale on Windows, `LC_ALL`, `LC_MESSAGES` or `LANG` on a Unix machine, and
English where none of them says -- which is what every other engine does, so
that a program run twice in the same environment is not given two different
answers. A server that formats for somebody else should set it rather than
inherit it.

Runaway recursion raises a catchable `RangeError` rather than overflowing the
goroutine stack. Deeply nested source is rejected at parse time for the same
reason — a goroutine stack overflow cannot be caught, so it would take the host
down — and so is deeply nested JSON, which `JSON.parse`, `JSON.stringify` and a
reviver all walk by recursion. A panic anywhere inside the engine is caught at
the `Eval` boundary and returned as `ErrInternal`: a host running untrusted code
must not be taken down by the code it is sandboxing.

Wall-clock bounds come from a `context.Context`, which the interpreter polls as
it executes, so an infinite loop is interrupted rather than hanging the process:

```go
ctx, cancel := context.WithTimeout(context.Background(), time.Second)
defer cancel()

_, err := rt.EvalContext(ctx, `while (true) {}`)
// errors.Is(err, context.DeadlineExceeded)
```

`EvalFileContext` does the same for a script with a name, which is what its
stack traces call it: `at main (app.js:12:5)` rather than `<eval>`.
`CallContext` and `CallWithThisContext` do it for a function called from Go
(see [Deadlines](embedding.md#deadlines)).

An interruption is deliberately **not** catchable from script, so a sandboxed
program cannot defeat its own timeout with `try`/`catch`. A catastrophically
backtracking regular expression fails with an error rather than stalling.

A runtime a sandbox runs should not be made `WithDebugger`: whoever attaches a
debugger to it can read and change anything the script can, and run code of
their own in it, code generation turned off or not. The inspector listens on
the loopback interface unless told otherwise; see [Debugging](debugging.md).

[go-intl-compat]: https://github.com/go-quickjs/go-intl/blob/main/compat.go
