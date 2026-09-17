# go-quickjs

A JavaScript engine for Go, reimplementing [QuickJS-NG] in pure Go.

No cgo, no WebAssembly, no C toolchain. It cross-compiles anywhere Go does.

```go
rt := quickjs.New()
defer rt.Close()

v, err := rt.Eval(`[1, 2, 3].map(x => x * 2).join("-")`)
fmt.Println(v) // 2-4-6
```

## Status

The language is substantially complete: expressions, closures, classes with
inheritance and private members, destructuring, generators, `async`/`await`,
Promises, regular expressions, modules with top-level `await`, Proxy and typed
arrays all work, and are exercised against [test262], the official ECMAScript
conformance suite.

98.6% of the test262 tests it runs pass. It is not finished: see
[Conformance](#conformance) for the measurement and
[Not implemented](#not-implemented) for the known gaps.

### Implemented

| Area | Notes |
|---|---|
| Expressions and operators | Full precedence, `??`, `?.`, `**`, BigInt |
| Variables | `var`, `let`, `const`, temporal dead zone, closures |
| Control flow | `if`, `for`, `for-in`, `for-of`, `while`, `do`, `switch`, labelled `break`/`continue` |
| Functions | Declarations, expressions, arrows, defaults, rest parameters, `arguments` |
| Classes | Constructors, methods, accessors, statics, `extends`, `super`, fields, private members, static blocks |
| Destructuring | Array and object patterns, defaults, nesting, rest elements |
| Exceptions | `throw`, `try`/`catch`/`finally`, stack traces |
| Iteration | Iterator protocol, spread, generators, `yield*` |
| Asynchrony | `Promise` with correct microtask ordering, `async`/`await` |
| Regular expressions | Backtracking engine: backreferences, lookahead, lookbehind, named groups, modifier groups, Unicode property escapes at Unicode 17, the `v` flag's set notation and properties of strings |
| Legacy | `with`, Annex B string methods, `escape`/`unescape`, sloppy-mode block functions |
| Modules | `import`/`export`, live bindings, cycles, namespace imports, dynamic `import()`, top-level `await` |
| Iterator helpers | `map`, `filter`, `take`, `drop`, `flatMap`, `reduce`, `toArray` and the rest, lazily |
| Unicode | Full case mappings including the final sigma, all four normalization forms, lone surrogates preserved end to end |
| Built-ins | `Object`, `Function`, `Array`, `String`, `Number`, `Boolean`, `Symbol`, `BigInt`, `Error`, `Math`, `JSON`, `Date`, `RegExp`, `Map`, `Set`, `Promise`, `Proxy`, `Reflect`, `ArrayBuffer`, `DataView`, typed arrays |
| Weak references | `WeakRef`, `FinalizationRegistry`, `WeakMap`, `WeakSet`, backed by Go's `weak.Pointer` and `runtime.AddCleanup`: a target really is released, and a registry really is called back |
| Reflection | `Proxy` with every trap and its invariants, `Reflect`, property descriptors, mapped `arguments` |
| Recent additions | Set operations, `Array.fromAsync`, `Object.groupBy`, `Promise.try`, `RegExp.escape`, `Error.isError`, `Math.sumPrecise`, `Uint8Array` base64 and hex |
| Eval | Direct `eval` runs in the caller's scope — its variables, `this`, `new.target` and `super`; indirect `eval` runs in global scope |
| Go interop | Function binding, marshalling, `context.Context` cancellation |

### Not implemented

`Intl`, `Temporal`, `Atomics`, `SharedArrayBuffer`, `ShadowRealm`, decorators,
resizable ArrayBuffers, `using` declarations, and the newer proposals test262
tracks.

Known semantic gaps, each covered by a test that documents it:

- A `WeakMap` or `WeakSet` value is held strongly, so a value that refers to its
  own key keeps that key alive. Breaking that cycle needs ephemeron marking,
  which Go's collector does not offer.
- `String.prototype.normalize` works from Unicode 13 decomposition tables, which
  is what the system the tables were generated from had. A character introduced
  after that decomposes to itself.

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

Measured coverage, as of the most recent run over the whole suite — 76,752 of
77,082 executed variants, 99.6%. A test tagged with a feature the engine does
not implement is skipped rather than counted, which is what the remaining 14,738
are. By area, worst first:

| Area | | Area | |
|---|---|---|---|
| `built-ins/AsyncFromSyncIteratorPrototype` | 94.7% | `built-ins/decodeURIComponent` | 98.2% |
| `language/identifier-resolution` | 95.5% | `built-ins/TypedArrayConstructors` | 98.4% |
| `built-ins/BigInt` | 96.1% | `built-ins/Function` | 98.6% |
| `language/destructuring` | 97.1% | `built-ins/Promise` | 98.9% |
| `built-ins/String` | 97.6% | `language/types` | 99.0% |
| `language/computed-property-names` | 97.9% | `built-ins/TypedArray` | 99.3% |
| `language/module-code` | 97.9% | `built-ins/Array` | 99.5% |
| `built-ins/Reflect` | 98.0% | `language/eval-code` | 99.6% |
| `built-ins/Proxy` | 98.1% | `language/arguments-object` | 99.6% |
| `built-ins/Math` | 98.2% | `built-ins/Object` | 99.6% |
| `built-ins/Error` | 98.2% | `built-ins/RegExp` | 99.7% |
| `built-ins/decodeURI` | 98.2% | `language/statements` | 99.8% |

`built-ins/Atomics` is the ten tests for `Atomics.pause`, which is the only part
of that API a single-threaded engine could offer and which is not implemented.

Useful flags:

```
-conformance.dir=language/expressions   # restrict to one area
-conformance.report=/tmp/failures.txt   # write every failure for triage
-conformance.timeout=2s                 # bound any one test
-conformance.workers=4                  # how many to run at once
```

The suite runs on every core, which takes a few minutes rather than well over an
hour.

## Calling Go from JavaScript

Set an ordinary Go function as a global. Arguments and results are converted
automatically.

```go
rt.Set("add", func(a, b int) int { return a + b })
rt.Eval(`add(1, 2)`) // 3
```

A function may return an `error`, which becomes a thrown JavaScript exception:

```go
rt.Set("mustBePositive", func(n int) (int, error) {
    if n < 0 {
        return 0, errors.New("negative")
    }
    return n, nil
})
rt.Eval(`try { mustBePositive(-1) } catch (e) { e.message }`) // "negative"
```

Taking a `*quickjs.Runtime` as the first parameter lets a Go function call back
into the engine:

```go
rt.Set("applyTwice", func(r *quickjs.Runtime, fn quickjs.Value, v int) (int, error) {
    once, err := fn.Call(v)
    if err != nil {
        return 0, err
    }
    twice, err := fn.Call(once.Int())
    return twice.Int(), err
})
rt.Eval(`applyTwice(n => n + 3, 1)`) // 7
```

## Reading values back

`Decode` follows the conventions of `encoding/json`, including struct tags. A
`js` tag takes precedence over a `json` tag, so a struct already annotated for
`encoding/json` works unchanged.

```go
var out struct {
    Name string   `js:"name"`
    Tags []string `js:"tags"`
}
v, _ := rt.Eval(`({name: "Ada", tags: ["math"]})`)
err := v.Decode(&out)
```

Decoding into `any` produces the natural Go form: `nil`, `bool`, `float64`,
`string`, `[]any` or `map[string]any`.

## Modules

Imports are resolved through a loader the host supplies. A runtime without one
rejects every import, which is the default:

```go
rt.SetModuleLoader(func(specifier, referrer string) (source, resolved string, err error) {
    b, err := os.ReadFile(filepath.Join(root, specifier))
    return string(b), specifier, err
})

ns, err := rt.EvalModule("main.js", `import {greet} from "./greet.js"; greet();`)
```

`EvalModule` returns the module's namespace, through which its exports can be
read. Bindings are live: an importer sees the exporter's current value, not a
copy taken at link time.

## Sandboxing

A runtime has no I/O, no network access, no timers, no filesystem and no module
loader unless the host adds them. There is no `require`, no `process`, no
`fetch`. `Date` reads the clock through an injectable hook rather than the
process clock.

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

Runaway recursion raises a catchable `RangeError` rather than overflowing the
goroutine stack. Deeply nested source is rejected at parse time for the same
reason — a goroutine stack overflow cannot be caught, so it would take the host
down. A panic anywhere inside the engine is caught at the `Eval` boundary and
returned as `ErrInternal`: a host running untrusted code must not be taken down
by the code it is sandboxing.

Wall-clock bounds come from a `context.Context`, which the interpreter polls as
it executes, so an infinite loop is interrupted rather than hanging the process:

```go
ctx, cancel := context.WithTimeout(context.Background(), time.Second)
defer cancel()

_, err := rt.EvalContext(ctx, `while (true) {}`)
// errors.Is(err, context.DeadlineExceeded)
```

An interruption is deliberately **not** catchable from script, so a sandboxed
program cannot defeat its own timeout with `try`/`catch`. A catastrophically
backtracking regular expression fails with an error rather than stalling.

## Concurrency

A `Runtime` is not safe for concurrent use. Give each goroutine its own, which
also isolates untrusted scripts from one another.

## Design notes

A few decisions worth knowing about if you read the source.

**Values are NaN-boxed into 24 bytes.** A `Value` is a `float64` plus an
interface. Real numbers live in the float; every other kind is a quiet NaN whose
payload encodes the type, with the pointer in the interface. Nothing allocates —
a pointer stored in an interface does not — and a type check is a mask and
compare. The invariant this rests on is that a genuine NaN must never collide
with a tag, so every number is normalized on the way in.

**The interpreter allocates nothing per call.** One contiguous slice backs both
locals and operands; a frame is a window into it. Neither that slice nor the
frame stack is ever grown, which is what makes it safe for a closure to hold a
pointer into a live frame and for the interpreter to hold a `*frame` across
nested calls. Exhausting either is the stack limit, reported as the same
"maximum call stack size exceeded" a browser gives.

**Strings are UTF-8 with an ASCII fast path, and concatenation builds a rope.**
Repeated `s += x` is how scripts build large strings, and copying each time is
quadratic. Ropes defer the copy; flattening is iterative because a rope from a
200,000-iteration loop is 200,000 deep.

**Lone surrogates are fully supported.** A JavaScript string is a sequence of
UTF-16 code units with no well-formedness requirement, so `"\uD83D"` is an
ordinary one-code-unit string. Go cannot represent one — `WriteRune` substitutes
U+FFFD — so `internal/wtf8` encodes them in WTF-8. Well-formed text is
bit-identical to UTF-8.

**The regexp engine backtracks, because it has to.** Go's `regexp` is RE2, which
buys linear time by refusing backreferences, lookahead and lookbehind — exactly
what JavaScript requires. Backtracking brings an exponential worst case, so the
matcher gives up after a step budget rather than letting a hostile pattern stall
the host.

**Generators save their frame rather than running on a goroutine.** A goroutine
per generator is simpler but leaks one for every generator never exhausted.
Saving the frame works because `yield` is only valid inside the generator's own
body, so at suspension its frame is the top of the stack.

**Promise reactions never run synchronously.** Settling queues them; the queue
drains between turns. That ordering guarantee is the point of the design.

**The Array methods read through the property protocol, not the element
slice.** Nearly all of them are generic — `Array.prototype.map.call(arguments,
f)` is supposed to work — and a hole has to fall through to the prototype while
an index redefined as an accessor has to be called. There is a fast path for the
case that dominates, a dense array whose element really is there: such an
element shadows anything on the prototype and cannot be an accessor, so taking
it directly is not observable.

**An arrow function captures its surroundings when the closure is made.** It has
no `this`, `new.target`, `super` or `arguments` of its own, so they are read
from the creating frame and the arrow ignores whatever its own call supplies.
Nesting needs no extra work: an arrow created inside another has already
inherited them.

## Benchmarks

On an Apple M5 Max:

```
BenchmarkEvalArithmetic-18  1.22µs/op   2632 B/op    23 allocs/op
BenchmarkFibonacci-18        806µs/op     24 B/op     1 allocs/op
BenchmarkPropertyAccess-18  1.79µs/op   3416 B/op    29 allocs/op
BenchmarkCallGoFunction-18  1.41µs/op   3200 B/op    34 allocs/op
```

`fib(20)` costs one allocation because the interpreter loop allocates nothing
per call; the other benchmarks include parsing and compiling their source each
iteration.

## License

MIT, matching the upstream project.

[QuickJS-NG]: https://github.com/quickjs-ng/quickjs
[test262]: https://github.com/tc39/test262
