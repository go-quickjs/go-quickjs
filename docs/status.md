# Status and conformance

## Implemented

| Area | Notes |
|---|---|
| Expressions and operators | Full precedence, `??`, `?.`, `**`, BigInt |
| Variables | `var`, `let`, `const`, temporal dead zone, closures |
| Control flow | `if`, `for`, `for-in`, `for-of`, `while`, `do`, `switch`, labelled `break`/`continue` |
| Functions | Declarations, expressions, arrows, defaults, rest parameters, `arguments`, proper tail calls in strict code |
| Classes | Constructors, methods, accessors, statics, `extends`, `super`, fields, private members, static blocks |
| Destructuring | Array and object patterns, defaults, nesting, rest elements |
| Exceptions | `throw`, `try`/`catch`/`finally`, and stack traces as V8 writes them, to the column, with `Error.captureStackTrace`, `Error.prepareStackTrace` and its `CallSite`s, and `Error.stackTraceLimit` |
| Resource management | `using` and `await using`, `DisposableStack`, `AsyncDisposableStack`, `SuppressedError` |
| Iteration | Iterator protocol, spread, generators, `yield*` |
| Asynchrony | `Promise` with correct microtask ordering, `async`/`await` |
| Regular expressions | Backtracking engine: backreferences, lookahead, lookbehind, named groups, modifier groups, Unicode property escapes at Unicode 17, the `v` flag's set notation and properties of strings |
| Legacy | `with` and all of Annex B: string methods, `escape`/`unescape`, `Date`'s `getYear` family, HTML-like comments, sloppy-mode block functions, `RegExp`'s legacy statics and `compile` |
| Modules | `import`/`export`, live bindings, cycles, namespace imports, dynamic `import()`, top-level `await`, import attributes with JSON, text and bytes modules, `import defer`, source phase imports |
| Iterator helpers | `map`, `filter`, `take`, `drop`, `flatMap`, `reduce`, `toArray` and the rest, lazily, with `includes`, `join`, `chunks`, `windows`, `Iterator.concat`, `Iterator.zip` and `Iterator.zipKeyed` |
| Unicode | Full case mappings including the final sigma, all four normalization forms, lone surrogates preserved end to end |
| Built-ins | `Object`, `Function`, `Array`, `String`, `Number`, `Boolean`, `Symbol`, `BigInt`, `Error`, `Math`, `JSON`, `Date`, `RegExp`, `Map`, `Set`, `Promise`, `Proxy`, `Reflect`, `ArrayBuffer` (resizable and immutable too), `SharedArrayBuffer`, `Atomics`, `DataView`, typed arrays |
| Temporal | `Instant`, `Duration`, `PlainDate`, `PlainTime`, `PlainDateTime`, `PlainYearMonth`, `PlainMonthDay`, `ZonedDateTime`, `Now`, non-ISO calendars, and IANA time-zone transitions |
| Internationalization | `Intl.Locale`, `NumberFormat`, `DateTimeFormat`, `Collator`, `PluralRules`, `ListFormat`, `RelativeTimeFormat`, `DisplayNames`, `Segmenter`, `DurationFormat`, from [go-intl], with CLDR data for every locale ICU has |
| Weak references | `WeakRef`, `FinalizationRegistry`, `WeakMap`, `WeakSet`, backed by Go's `weak.Pointer` and `runtime.AddCleanup`: a target really is released, and a registry really is called back |
| Reflection | `Proxy` with every trap and its invariants, `Reflect`, property descriptors, mapped `arguments` |
| Recent additions | Set operations, `Array.fromAsync`, `Object.groupBy`, `Promise.try`, `Promise.allKeyed`, `RegExp.escape`, `Error.isError`, `Math.sumPrecise`, `Uint8Array` base64 and hex, `Map` and `WeakMap`'s `getOrInsert`, `JSON.rawJSON` and a reviver's source text, `Atomics.pause` and `Atomics.waitAsync`, `ShadowRealm` |
| Eval | Direct `eval` runs in the caller's scope — its variables, `this`, `new.target` and `super`; indirect `eval` runs in global scope |
| Go interop | Function binding, marshalling, `context.Context` cancellation |

## Internationalization

Every `Intl` API and its locale data are built in, from [go-intl], and so is
`Date`'s local time, from its `date` package, which is V8's `Date`: the offset
cache, the strings `Date` writes and `Date.parse`. Time-zone names, including
generic and historical names, are written in the requested language.
`new Date().toString()` likewise ends with the localized zone name -- for
example, `(Mitteleuropäische Normalzeit)` in winter and
`(Mitteleuropäische Sommerzeit)` in summer when using a German locale and a
Central European time zone. Zone arithmetic uses the same bundled tz release
as those names, so results do not depend on whether the host operating system
has installed newer or older zone rules. The host's zone is found as ICU finds
it, and `TZ` names it where Node reads it, on Windows too.

`Temporal` is go-intl's as well: its `temporal` package is a port of the
temporal_rs 0.2.3 that Node 26 builds Temporal from, with ICU4X's calendars,
and the engine reads property bags and options as V8 does before it calls
temporal_rs, with V8's messages. go-intl holds it to Node's recorded answers
for every type's methods and all sixteen calendars.

Sorting follows the Unicode algorithm with each language's tailoring,
including Chinese pinyin, stroke and zhuyin order, Japanese kana and Han
order, Korean order, and the named Unihan and search-jamo collations.
`Intl.Segmenter` carries ICU's word dictionaries for Chinese, Japanese, Thai,
Lao, Khmer and Burmese. Non-Gregorian calendars carry their own localized
date layouts, month contexts and era placement rather than borrowing the
Gregorian layout.

## Not implemented

Decorators.

A runtime can hold more than one realm, each with its own global object and
intrinsics, and a function runs in the realm it was made in whoever calls it;
test262's cross-realm tests run against it, and `ShadowRealm` and node's `vm`
are built on it. A `SharedArrayBuffer`'s memory can be shared between
runtimes running on different goroutines, each an agent of its own: `Atomics`
operations on it are atomic in fact, and `Atomics.wait` in one agent is woken
by `Atomics.notify` in another -- or, for `Atomics.waitAsync`, has its promise
settled on its own goroutine, by the host's event loop when it has one.
test262's multi-agent tests run that way; a host has no API to share memory
yet.

Two things are left out on purpose, as QuickJS-NG leaves them out or as Node
behaves:

- A sloppy-mode function has no `caller`, and its `arguments` are not
  reachable from outside it: reading either throws, as the standard's
  `Function.prototype` accessors do.
- An error's `stack` is an own accessor, as in V8, whose text is made when it
  is first read, rather than the accessor on `Error.prototype` that a proposal
  describes.

In the standard library: node's own streams (the web's are here instead, and
everything that takes a stream takes those), brotli, and BYOB readers. A
decompression stream gathers its input rather than being pulled through, since
nothing here can pull it without taking the runtime onto another goroutine.

Known semantic gap, covered by a test that documents it:

- A `WeakMap` value is held strongly, so a value that refers to its own key
  keeps that key alive. Breaking that cycle needs ephemeron marking, which Go's
  collector does not offer.

## Conformance

The engine is tested against test262. The suite is not vendored; point the
runner at a checkout:

```
git clone --depth 1 https://github.com/tc39/test262 /tmp/test262
TEST262_DIR=/tmp/test262 go test ./conformance -timeout 120m
```

The runner honours each test's frontmatter: the harness files to include, the
strict and sloppy variants, the expected-failure phase and type, and the feature
tags. A test tagged with a feature the engine does not implement is skipped
rather than counted against it.

By default it runs `language`, `built-ins`, `intl402` and `annexB`; `staging`
holds proposals too early to claim, and `harness` tests the suite's own helpers.

Measured coverage, as of the most recent run: 99,595 variants pass and none
fail. The other 342 are skipped rather than counted: a test tagged with a
feature the engine does not implement, or one that asks the host for a second
realm or an agent, is testing something that was never claimed.

Where test262 and current engines disagree, the engine follows the standard
and a test records the difference. Two of Annex B's block-function rules are
examples: a block-level function named `arguments` does not replace the
arguments object, and one nested in a block that already declares a function
of that name stays in its block. V8 does both, and so does the engine under
`WithNodeQuirks`.

Useful flags:

```
-conformance.dir=language/expressions   # restrict to one area
-conformance.report=/tmp/failures.txt   # write every failure for triage
-conformance.timeout=2s                 # bound any one test
-conformance.workers=4                  # how many to run at once
-conformance.force-feature=proposal     # run a normally skipped feature
```

The suite runs on every core, which takes a few minutes rather than well over an
hour.

[go-intl]: https://github.com/go-quickjs/go-intl
