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

**This is a work in progress and is not ready for production use.** The core
language runs, but several major features are missing. The table below is
current; anything marked missing will fail with a clear error rather than
silently misbehaving.

### Working

| Area | Notes |
|---|---|
| Expressions and operators | Full precedence, `??`, `?.`, `**`, BigInt arithmetic |
| Variables | `var`, `let`, `const`, temporal dead zone, closures |
| Control flow | `if`, `for`, `for-in`, `for-of`, `while`, `do`, `switch`, labelled `break`/`continue` |
| Functions | Declarations, expressions, arrows, defaults, recursion, named function expressions |
| Closures | Per-iteration `let` capture, shared bindings |
| Objects | Literals, computed keys, getters/setters, spread, `__proto__` |
| Classes | Constructors, methods, accessors, statics, prototype chain |
| Arrays | Literals, spread, and most of `Array.prototype` |
| Destructuring | Array and object patterns, defaults, nesting |
| Strings | Most of `String.prototype`, templates, full lone-surrogate support |
| Exceptions | `throw`, `try`/`catch`, stack traces |
| Built-ins | `Object`, `Function`, `Array`, `String`, `Number`, `Boolean`, `Symbol`, `Error`, `Math`, `JSON` |
| Go interop | Function binding, marshalling, `context.Context` cancellation |

### Not yet implemented

| Area | Status |
|---|---|
| `RegExp` | Not implemented; a literal raises an error |
| Generators, `async`/`await`, `Promise` | Not implemented |
| `Map`, `Set`, `WeakMap`, `WeakSet` | Not implemented |
| `Date` | Not implemented |
| `Proxy`, `Reflect` | Not implemented |
| Typed arrays, `ArrayBuffer` | Not implemented |
| Modules (`import`/`export`) | Not implemented |
| Class inheritance (`extends`, `super`) | Parses, not compiled |
| Class fields and private members | Parses, not compiled |
| Rest parameters and rest in destructuring | Parses, not compiled |
| Spread in call arguments | Parses, not compiled |
| `try`/`finally` | Parses, not compiled |
| `arguments` object | Not implemented |
| Tagged templates | Not implemented |
| Top-level `let`/`const` across `Eval` calls | Compiled as program locals, so they do not persist; `var` does |
| `eval`, the `Function` constructor | Deliberately disabled — see Sandboxing |

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

## Sandboxing

A runtime has no I/O, no network access, no timers and no filesystem unless the
host adds them. There is no `require`, no `process`, and no `fetch`. The
`Function` constructor is disabled, so a script cannot compile new code from a
string and escape static review.

Bounds are set at construction:

```go
rt := quickjs.New(
    quickjs.WithMemoryLimit(64<<20),
    quickjs.WithStackSize(1<<16),
    quickjs.WithMaxCallDepth(1000),
)
```

Runaway recursion raises a catchable `RangeError` rather than overflowing the
goroutine stack.

Wall-clock bounds come from a `context.Context`, which the interpreter polls as
it executes, so an infinite loop is interrupted rather than hanging the process:

```go
ctx, cancel := context.WithTimeout(context.Background(), time.Second)
defer cancel()

_, err := rt.EvalContext(ctx, `while (true) {}`)
// errors.Is(err, context.DeadlineExceeded)
```

An interruption is deliberately **not** catchable from script, so a sandboxed
program cannot defeat its own timeout with `try`/`catch`.

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

**Strings are UTF-8 with an ASCII fast path, and concatenation builds a rope.**
Repeated `s += x` is how scripts build large strings, and copying each time is
quadratic. Ropes defer the copy; flattening is iterative because a rope from a
200,000-iteration loop is 200,000 deep.

**Lone surrogates are fully supported.** A JavaScript string is a sequence of
UTF-16 code units with no well-formedness requirement, so `"\uD83D"` is an
ordinary one-code-unit string. Go cannot represent one — `WriteRune` substitutes
U+FFFD — so `internal/wtf8` encodes them in WTF-8. Well-formed text is
bit-identical to UTF-8.

**Property keys are interned to integers**, with array indices encoded in the
key itself so that `a[i]` over a large range does not grow the intern table.
Objects keep properties in insertion order and only build a lookup map past
eight properties, below which scanning a contiguous slice wins.

**The interpreter allocates nothing per call.** One contiguous slice backs both
locals and operands; a frame is a window into it. The slice is never grown,
which is what makes it safe for a closure to hold a pointer into a live frame,
and it gives the stack limit for free.

## Benchmarks

On an Apple M5 Max:

```
BenchmarkFibonacci-18       1000    1206022 ns/op    24 B/op    1 allocs/op
BenchmarkEvalArithmetic-18  919164     1161 ns/op  2536 B/op   22 allocs/op
BenchmarkPropertyAccess-18  695341     1723 ns/op  3320 B/op   28 allocs/op
BenchmarkCallGoFunction-18  930800     1305 ns/op  3104 B/op   33 allocs/op
```

`fib(20)` costs one allocation because the interpreter loop itself allocates
nothing; the arithmetic benchmarks include parsing and compiling the source
each iteration.

## License

MIT, matching the upstream project.

[QuickJS-NG]: https://github.com/quickjs-ng/quickjs
