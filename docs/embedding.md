# Embedding the engine

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

That exception is an `Error`. To throw one of the other kinds, return what
`ThrowTypeError`, `ThrowRangeError`, `ThrowReferenceError`,
`ThrowSyntaxError`, `ThrowEvalError` or `ThrowURIError` makes, each
formatting its message as `fmt.Errorf` does (`ThrowError` makes an `Error`):

```go
rt.Set("setVolume", func(n int) error {
    if n < 0 || n > 11 {
        return rt.ThrowRangeError("volume must be between 0 and 11, not %d", n)
    }
    return nil
})
rt.Eval(`try { setVolume(12) } catch (e) { e instanceof RangeError }`) // true
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

### Sequences

A Go function that returns an `iter.Seq` hands script an iterator, which
`for-of`, spread, destructuring and the iterator helpers take. Script asks
for one value at a time, and the sequence runs only while it waits for the
next:

```go
rt.Set("countdown", func(n int) iter.Seq[int] {
    return func(yield func(int) bool) {
        for i := n; i > 0; i-- {
            if !yield(i) {
                return
            }
        }
    }
})
rt.Eval(`[...countdown(3)]`)                      // [3, 2, 1]
rt.Eval(`countdown(10).map(n => n * 2).take(2).toArray()`) // [20, 18]
```

A script that stops early -- `break`, `return`, a destructuring that takes
fewer values than the sequence has, a helper such as `take` -- stops the
sequence: its `yield` returns false, as it does for a Go `range` that
breaks, and its deferred calls run. So does closing the runtime, and so does
the garbage collector for an iterator script drops halfway, on the runtime's
goroutine when it next runs its jobs. An `iter.Seq2` gives `[key, value]`
arrays, as a `Map`'s entries are. A sequence that calls back into script
must not ask its own iterator for a value while it produces one; that call
throws a `TypeError`, as resuming a running generator does.

### Async functions written in Go

`rt.Go` runs a function on a goroutine of its own and returns a promise for
its result, which a Go function hands script for work that finishes later:

```go
rt.Set("fetchUser", func(id int) quickjs.Value {
    return rt.Go(func(ctx context.Context) (any, error) {
        return db.LoadUser(ctx, id) // runs off the runtime's goroutine
    })
})
rt.Eval(`const user = await fetchUser(7)`)
```

The result is converted as `Set` converts one, and an error rejects the
promise as a thrown one would; both happen on the runtime's goroutine when
it next runs its jobs, which keeps the runtime busy until then. The function
is given the runtime's `Context`, which `Close` cancels. It runs on its own
goroutine, so it takes what it needs from script as Go values before it
starts, and touches neither the runtime nor its values. For work that
reports more than once, such as a socket, see
[AsyncWork](hosting.md#work-that-finishes-on-other-goroutines).

## Calling JavaScript from Go

A function the script defines is a `Value`, and `Call` calls it:

```go
rt.Eval(`function greet(name) { return "hello, " + name }`)
greet, _ := rt.Get("greet")
v, err := greet.Call("Ada") // v.String() == "hello, Ada"
```

Arguments are converted as `Set` converts values: numbers, strings, booleans,
slices, maps, structs and functions all work, and a `Value` is passed as it is.
The result is a `Value`. Read it with `String`, `Int`, `Float` and `Bool`, or
`Decode` it into a Go type (see [Reading values back](#reading-values-back)):

```go
type User struct {
    Name string `js:"name"`
    Age  int    `js:"age"`
}
older, _ := rt.Eval(`(u => ({...u, age: u.age + 1}))`)
v, _ := older.Call(User{Name: "Ada", Age: 36})
var u User
v.Decode(&u) // {Name:Ada Age:37}
```

A Go function passed as an argument is a callback the script can call:

```go
mapFn, _ := rt.Eval(`((xs, f) => xs.map(f))`)
v, _ := mapFn.Call([]int{1, 2, 3}, func(x int) int { return x * 10 }) // [10, 20, 30]
```

### Finding the function

- `rt.Get` reads a property of the global object. That covers `function` and
  `var` declarations and anything assigned to `globalThis`.
- A top-level `class`, `let` or `const` is not a property of the global object,
  so `rt.Get` doesn't see it. Evaluate its name instead: `rt.Eval("Point")`.
- A method is a property of its object, read with `Get`. Call it with
  `CallWithThis` so that `this` is the object.
- `Get` of a name that doesn't exist returns `undefined`, not an error. Calling
  that gives the error `quickjs: undefined is not a function`, so check
  `IsFunction` first when the name may be missing.

```go
counter, _ := rt.Eval(`({n: 0, add(k) { return this.n += k }})`)
add, _ := counter.Get("add")
add.CallWithThis(counter, 5) // 5
add.CallWithThis(counter, 2) // 7

rt.Eval(`class Point { constructor(x, y) { this.x = x; this.y = y } }`)
Point, _ := rt.Eval("Point")
p, _ := Point.New(3, 4) // new Point(3, 4)
```

### Deadlines

`Call` has no deadline, so a function that never returns blocks the caller.
`CallContext` and `CallWithThisContext` take a context, as `EvalContext` does.
When the context ends, the function stops and the error wraps `ctx.Err()`:

```go
ctx, cancel := context.WithTimeout(context.Background(), time.Second)
defer cancel()
v, err := handler.CallContext(ctx, req)
// errors.Is(err, context.DeadlineExceeded) when it ran too long
```

A Go function the script called may call back into the script with
`CallContext`. That call stops at the script's deadline too, even if its own
context has none.

### Async functions

An `async` function returns a promise. `Await` waits for it to settle,
running the runtime's jobs and its host work meanwhile, as an event loop
would, and returns its value, or its rejection as the error a call that
threw would return:

```go
rt.Eval(`async function fetchUser(id) { await null; return {id, name: "Ada"} }`)
fetchUser, _ := rt.Get("fetchUser")
p, _ := fetchUser.Call(7)
user, err := p.Await(ctx)
// user.Get("name") is "Ada"
```

`Await` takes any value, as `await` does: a thenable is followed, and
anything else is its own result. It stops when `ctx` does, and returns
`ErrNeverSettles` for a promise the runtime has nothing left to settle. A
promise it waits for counts as handled. It is for the host between calls: a
Go function that script called gets an error from it rather than running the
runtime's jobs in the middle of the script. Without it, a `then` and
`RunJobs` do the same by hand.

### Errors

A JavaScript exception that reaches Go is a `*quickjs.Error`. `Value` returns
what was thrown (usually an `Error` object, but a script may throw anything),
and `Stack` returns the JavaScript stack trace.

When the exception came from a Go function's error, `errors.Is` and `errors.As`
find that Go error through the `*quickjs.Error`. This works whether the script
let the exception through or caught it and rethrew the same object:

```go
rt.Set("load", func(key string) (string, error) {
    return "", fmt.Errorf("load %s: %w", key, errNotFound)
})
rt.Eval(`function handler(req) { return load(req.key) }`)
handler, _ := rt.Get("handler")

_, err := handler.Call(map[string]any{"key": "a"})
var jsErr *quickjs.Error
switch {
case errors.Is(err, errNotFound): // the Go error behind the exception
case errors.As(err, &jsErr):      // any other exception: jsErr.Value(), jsErr.Stack()
case err != nil:                  // not an exception; see below
}
```

The errors that are not exceptions are:

| Error | Cause |
|---|---|
| wraps `context.DeadlineExceeded` or `context.Canceled` | `CallContext`'s context, or the script's, ended |
| `quickjs.ErrMemoryLimit` | the script went over `WithMemoryLimit` |
| `quickjs.ErrClosed` | the runtime was closed |
| `quickjs.ErrInternal` | a bug in the engine; please report it |
| `quickjs: … is not a function` | the value called isn't a function |

The script can't catch any of these. A Go function's own deadline error is
different: it is thrown as an ordinary exception, so if you need to tell "my
call ran out of time" apart from "a Go function reported a timeout", check for
`*quickjs.Error` first.

Source that doesn't compile -- given to `Eval`, `EvalFile`, `EvalModule`,
`Compile`, or a module a loader returned -- is a `*quickjs.SyntaxError`, which
says where: `Position` returns the name the source was compiled under and the
line and column, placed as `WithOffset` placed the source, and the message ends
with them, as in `unexpected punctuator ";" (main.js:3:9)`. A Go function that
returns one throws a `SyntaxError` with that message, so a host compiling a
file for a script -- a `require` -- reports the file's own position.

A `Value` belongs to its runtime, so call it on the goroutine that uses that
runtime (see [Concurrency](#concurrency)).

## Compiling a script once

`Compile` parses and compiles a script into a `*Program`, which any runtime can
then run with `RunProgram`. This is the same idea as goja's `Compile`. A server
that gives each request its own runtime pays for parsing once instead of once
per request:

```go
var handler = func() *quickjs.Program {
    p, err := quickjs.Compile("handler.js", handlerSource)
    if err != nil {
        panic(err)
    }
    return p
}()

func serve(req Request) (string, error) {
    rt := quickjs.New()
    defer rt.Close()
    v, err := rt.RunProgram(handler)
    // ...
}
```

- A `Program` holds no runtime's values, so it can be shared between
  goroutines and run on several runtimes at once.
- `quickjs.WithStrict()` makes the whole script strict code, as a leading
  `"use strict"` would.
- The name is what stack traces call the script, as in `EvalFile`.
- A syntax error is a `*quickjs.SyntaxError`, returned by `Compile`.
- `RunProgramContext` takes a deadline, as `EvalContext` does.
- A program runs as a script, like `Eval`. Running it twice on one runtime
  redeclares its top-level `class`, `let` and `const`, which is a SyntaxError,
  as evaluating the same source twice would be.
- `quickjs.WithOffset(line, column)` places the source within a larger file,
  such as a script in an HTML page, so that stack traces and syntax errors
  give positions in the whole file.
- A program is compiled as the standard has it. A runtime `WithNodeQuirks`
  compiles it once more, the first time one runs it, so that it runs as that
  runtime would compile it. `rt.Compile` compiles as that runtime parses, so
  that source only V8 accepts compiles where `Eval` would accept it.

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

### The zero Value

The zero `quickjs.Value` -- a field the host never set, or what a call that
failed returned -- is the number zero, not undefined or null. Handed to the
engine it is `0`, by any route: `Set`, a call's arguments or receiver, a Go
function's result, a struct's field. Its own methods agree:

```go
var v quickjs.Value
v.Kind()     // quickjs.KindNumber
v.String()   // "0"
v.Float()    // 0
v.Decode(&n) // n is 0
v.Equal(x)   // as 0 == x
```

It belongs to no runtime, so what needs one -- `Get`, `Set`, `Call`, `New` --
returns `quickjs.ErrClosed`. A struct field that may be absent is better a
`*quickjs.Value`, which a nil pointer turns into `null`; or check the error
before passing a `Value` on.

## Extending a runtime

A runtime starts with the language and nothing else. What a script can reach is
what the host puts there — a global, or a module it has to import:

```go
rt.Set("add", func(a, b int) int { return a + b })

rt.SetModule("storage", map[string]any{
    "get":     func(key string) string { return store[key] },
    "set":     func(key, value string) { store[key] = value },
    "default": map[string]any{"name": "storage"},
})
rt.EvalModule("main.js", `import {get} from "storage"; get("k")`)
```

A module registered this way answers to its name ahead of any loader, and works
in a runtime with no loader at all.

Anything that finishes later hands back a promise the host settles:

```go
rt.Set("readLater", func(name string) *quickjs.Promise {
    p := rt.NewPromise()
    go func() {
        b, err := os.ReadFile(name)
        loop.Post(func() {           // back on the runtime's goroutine
            if err != nil {
                p.RejectError(err)
                return
            }
            p.Resolve(string(b))
        })
    }()
    return p
})
```

`RejectError` rejects with what returning its error would throw: an `Error`
carrying a Go error's message, or the error of another kind a `Throw*Error`
method made, as in `p.RejectError(rt.ThrowTypeError("not a file: %s", name))`.

`NewObject`, `NewArray`, `NewBytes`, `NewError` and `Throw` build the values a
marshalled Go value cannot express; `Value.Bytes` reads a typed array back.
`OnUnhandledRejection` reports a promise nobody took.

A host that emulates a browser's document can make its `document.all`:
`NewHTMLDDA` returns an object with Annex B's [[IsHTMLDDA]] slot, which
`typeof` calls `"undefined"`, which is falsy, and which is `==` to `null` and
`undefined`, while everything else sees an ordinary object. Given a Go
function, it is callable too.

## Realms

A runtime evaluates in the realm it was made with, and can make more: each has
a global object and built-ins of its own, and shares the runtime's stack, job
queue and module loader. A value made in one is an ordinary `Value` that code
in another can be handed; a function runs in the realm it was made in,
whoever calls it.

```go
re, _ := rt.NewRealm()
re.Set("log", func(s string) { fmt.Println(s) }) // a function of that realm
re.Eval(`log(String(Array.isArray([])))`)
```

A realm made `WithSandbox` is a context, as `node:vm`'s `createContext` makes
one: its global names are the sandbox object's properties first, and what its
scripts declare globally lands on the sandbox. `RunProgram` and
`RunProgramContext` run a compiled program in a realm:

```go
sandbox, _ := rt.Eval(`({count: 1})`)
re, _ := rt.NewRealm(quickjs.WithSandbox(sandbox))
p, _ := quickjs.Compile("page.js", `var next = count + 1`)
re.RunProgram(p) // sandbox.next is 2
```

Called from a Go function inside a running script, as `node:vm` calls them,
they run in that script's turn, and a context of their own that ends stops that
run alone: it returns an error wrapping the context's, and the script that made
the call runs on. `Value.CallContext` and `EvalContext` behave the same.
`node:vm`, in the standard library, is built on these.

## Modules

Imports are resolved through a loader the host supplies. A runtime without one
rejects every import, which is the default:

```go
rt.SetModuleLoader(func(specifier, referrer string) (source, resolved string, err error) {
    base := root
    if referrer != "" {
        base = filepath.Dir(referrer)
    }
    resolved = filepath.Join(base, specifier)
    b, err := os.ReadFile(resolved)
    return string(b), resolved, err
})

main := filepath.Join(root, "main.js")
ns, err := rt.EvalModule(main, `import {greet} from "./greet.js"; greet();`)
```

The referrer is the name of the code the import is written in, so a relative
specifier can be resolved against it: the name a module was loaded under -- the
loader's resolved name, or `EvalModule`'s specifier -- or the name a script was
compiled under, by `EvalFile` or `Compile`. It is the same for a static import
and an `import()`, wherever that runs: in a function called from another
module, after an `await`, or in code `eval` or `new Function` compiled, which
resolves as the code that called them does. Code `Eval` runs has no name, and
its referrer is empty.

`EvalModule` returns the module's namespace, through which its exports can be
read. Bindings are live: an importer sees the exporter's current value, not a
copy taken at link time.

## Concurrency

A `Runtime` is not safe for concurrent use. Give each goroutine its own, which
also isolates untrusted scripts from one another. Runtimes share nothing unless
a host shares it: a `SharedArrayBuffer`, or a message, which `stdlib`'s workers
post.
