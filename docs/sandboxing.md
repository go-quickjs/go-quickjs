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
process has allocated a quarter of the limit since it last did, and before
any allocation big enough to cross the limit at once.

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

[QuickJS-NG]: https://github.com/quickjs-ng/quickjs
[test262]: https://github.com/tc39/test262
[goja]: https://github.com/dop251/goja
[v8-v7]: https://github.com/mozilla/arewefastyet/tree/master/benchmarks/v8-v7
[go-intl]: https://github.com/go-quickjs/go-intl
[go-intl-compat]: https://github.com/go-quickjs/go-intl/blob/main/compat.go
[arm-aor]: https://github.com/ARM-software/optimized-routines
