# Debugging

A runtime made `WithDebugger` can be debugged as Node is: Chrome's DevTools,
VS Code, or any other client of the
[Chrome DevTools Protocol](https://chromedevtools.github.io/devtools-protocol/)
attaches to it, sets breakpoints, steps, looks at the stack and its
variables, evaluates code in a paused frame, and sees what the console
writes. It is for development: a debugger can read and change anything the
script can, and run code in it.

## With qjs

`qjs` takes Node's flags:

```
qjs --inspect app.js            # a debugger may attach while it runs
qjs --inspect-wait app.js       # wait for one before running
qjs --inspect-brk app.js        # wait, and stop at the first statement
qjs --inspect=9230 app.js       # another port; or host:port
```

It prints where it listens:

```
Debugger listening on ws://127.0.0.1:9229/0f2d5e1c-6a1b-4f5e-9d2a-3c4b5a6f7e8d
For help, see: https://nodejs.org/en/docs/inspector
```

- **Chrome:** open `chrome://inspect`, which finds a target at
  `localhost:9229` as it finds Node's, and click *inspect*.
- **VS Code:** attach to it with a launch configuration of type `node` and
  request `attach`, or run the program from VS Code's JavaScript Debug
  Terminal with `--inspect-brk`.

A file is named to the debugger by its file URL, as Node names it, so that a
breakpoint set in the editor's copy of a file lands in the program's. A
worker is a target of its own, listed beside the program's. The standard
library's own scripts -- the console, the web APIs, the module loader -- are
not shown, and a step never stops in them.

The server listens on `127.0.0.1` unless it is told another address, and
refuses a request whose `Host` header is neither an address nor `localhost`, so
that a web page cannot reach it by DNS rebinding. Listening anywhere else lets
whoever can reach the port run code as the program.

## In a host

A host makes the runtime with `quickjs.WithDebugger()`, and serves it with the
[inspector](../inspector) package. One server serves any number of runtimes,
each attached as a target of its own:

```go
rt := quickjs.New(quickjs.WithDebugger())
defer rt.Close()

srv, err := inspector.Listen("127.0.0.1:9229")
if err != nil {
	return err
}
defer srv.Close()

target, err := srv.Attach(rt, inspector.Options{Title: "app.js"})
if err != nil {
	return err
}
log.Println("debugger at", target.WebSocketURL())

// Optionally, wait for a client, and stop at the first statement.
if err := target.WaitForDebugger(ctx, true); err != nil {
	return err
}
_, err = rt.EvalFile("app.js", src)
```

`Attach` and `WaitForDebugger` are called on the runtime's goroutine, as
every runtime method is. What a client asks is done there too:

- while the runtime is paused, by the pause, which serves the clients until
  one says to go on;
- while it runs a script, at its next interrupt check;
- while it waits for work, as a job of the host's event loop -- the standard
  library's, or any loop that runs the runtime's async work.

A host that runs no loop has a client's requests done the next time a script
runs.

A target ends when its runtime is closed, or when `Detach` is called; closing
the server detaches every target. A client that disconnects takes its
breakpoints with it, and a pause it was attending to goes on.
`Options.ScriptURL` names scripts to clients -- `qjs` makes a file URL of an
absolute path -- and `Options.URL` and `Options.Title` say what the target is.

## Source maps

A script's last `//# sourceMappingURL=` comment is told to the debugger as V8
tells it, and the debugger does the mapping: VS Code reads the map and shows
the original source -- TypeScript, say -- and breakpoints set in it land in the
compiled script; Chrome's DevTools does the same with a map it can load, which
an inline one, a `data:` URL, always is. As with Node, the target does not load
a map for DevTools (`Network.loadNetworkResource`).

Code an `eval` or a `Function` call compiles may name itself with
`//# sourceURL=name`, as bundlers and tools do: a debugger lists it by that
name, a breakpoint set by the name stops in it, and a stack trace calls it so,
as V8's do -- `at eval (generated.js:1:7)` rather than where it was evaluated.

### Stack traces

An error's stack can be mapped too, as Node's `--enable-source-maps` maps it,
with or without a debugger: `qjs --enable-source-maps app.js`, or a runtime
made `WithSourceMaps`. Each frame in a script that names its map is placed in
the source it was compiled from, and called by the name the map gives it:

```
Error: too big: 150
    at add (/proj/src/app.ts:3:11)
    at Calc.push (/proj/src/app.ts:9:34)
```

where the compiled script alone says `at a (/proj/dist/app.js:5:15)`. A host
gives the loader that reads a map: `nil` reads only one inline in the script,
and `quickjs.ReadSourceMap` reads them as Node does, from the file a relative
URL names beside the script too, and none of a script in `node_modules`:

```go
rt := quickjs.New(quickjs.WithSourceMaps(quickjs.ReadSourceMap))
```

A map is read the first time a stack in its script is written, once; until
then, and in a runtime made without the option, nothing is read or kept. A
script's own `Error.prepareStackTrace` gets the frames as they are, as Node's
does.

## What it costs

A runtime made without `WithDebugger` compiles none of this and pays nothing
for it.

A runtime made with it compiles every script, module, program and eval for a
debugger: each statement, each loop's test, a `for` loop's update and a
`for`-`of` or `for`-`in` head is a place the code can stop. The program runs:

- **in the interpreter only,** without the tier that turns a function's
  bytecode into closures;
- **with a test at each statement,** until a debugger asks to stop somewhere.

It takes 1.1 to 1.5 times as long as it would otherwise, and with any
breakpoint set, 1.1 to 1.9 times. A closure keeps more of its surroundings
alive, since a debugger may show any of them.

## What it supports

| | |
|---|---|
| Breakpoints | by URL, by URL pattern, in a script; conditional; set before the script loads; turned all on or off |
| Stepping | into, over, out; pause on request |
| Exceptions | pause on all, or on uncaught: those no `catch` on the stack catches |
| The stack | each frame's function, location, `this` and scopes -- local, closure, `with`, the script's top-level declarations, global |
| Values | objects and their properties, accessors without calling them, prototypes, a promise's state, a proxy's target and handler |
| Evaluation | in a paused frame, reading and changing its variables; in global scope; `callFunctionOn`; changing a variable |
| The console | each call, with its arguments and where it was made |
| Scripts | their source, the places a breakpoint can go, a source map's URL, a name given by `//# sourceURL=` |

Not yet: async stack traces, restarting a frame, live editing.

Profiling is out of scope for now: the `Profiler` and `HeapProfiler` domains,
CPU profiles and heap snapshots. go-quickjs is first an engine to embed, and a
host profiles the program it is part of with Go's own tools -- `pprof` sees
the time and the memory a script takes as the engine's -- while a client that
asks the inspector to profile is refused.

Where V8 can stop at an expression inside a statement, the engine stops at the
statement; and a closure's scope shows every variable around it, where V8 shows
only those the closure uses.
