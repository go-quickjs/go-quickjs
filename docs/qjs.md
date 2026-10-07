# The qjs command

```
qjs script.js arg1 arg2      run a file
qjs -e 'console.log(1 + 1)'  run an expression
qjs                          read from a prompt
cat script.js | qjs -        run what arrives on standard input
```

A file is run as node runs it. One named `.mjs`, or `.js` in a package whose
`package.json` says `"type": "module"`, is an ES module; one named `.cjs`, or
`.js` in a package of type `"commonjs"`, is CommonJS; and a `.js` file in a
package of neither is a module if it uses `import` or `export`, and CommonJS
if not.

CommonJS has node's `require` and `module`. `require` finds files as node does
-- a path, with `.js` or `.json` added, or a directory's `package.json` main or
index file -- and packages in `node_modules`, from the requiring file's
directory up, through their `"exports"` with the `require` condition; `#name`
reaches the package's `"imports"`. node's own modules are there by their names,
with or without `node:`: `require("fs")`, `require("node:path")`. JSON files,
`require.cache`, `require.resolve`, `require.main` and cycles behave as they do
in node, and so do its errors, by their codes: `MODULE_NOT_FOUND`,
`ERR_PACKAGE_PATH_NOT_EXPORTED` and the rest.

The two kinds meet as they do in node. An ES module imports a CommonJS file
as its `module.exports`, the default export, with the names its source exports
named exports too; a package gives it what its `import` condition says. A
CommonJS file may `require` an ES module, which is evaluated then and there --
unless it awaits at the top level, which is `ERR_REQUIRE_ASYNC_MODULE`.
`createRequire` from `node:module` gives an ES module a `require` of its own,
and `import.meta` has `url`, `filename`, `dirname` and `resolve`.

Code that is not a file -- `-e`, standard input, the prompt, a worker given
code with `eval` -- has a `require` too, which resolves from the working
directory. `--script` runs a file as a classic script instead, whose top-level
`var` is a global and which has no `require`; `--module` runs it as a module.

A script can do nothing outside the process until the command line says it may,
because the engine has no ambient authority to withhold:

```
qjs --allow-read=. build.js             read files under this directory
qjs --allow-write=/tmp --allow-read=/tmp generate.js
qjs --allow-net=api.example.com fetch.js reach one host, and serve on it
qjs --allow-env deploy.js                read the environment
qjs --allow-run=git release.js           start programs, or only some
qjs -A script.js                         all of it, for code you trust
```

A worker is started as in node -- `new Worker("./work.mjs")` from
`node:worker_threads`, or the web's `new Worker(url)` -- and is given what the
program was given: the same flags hold for it.

A script that reaches for something it was not given is told which flag would
have given it, rather than finding a hole where a function should be:

```
$ qjs -e 'fetch("https://example.com")'
uncaught (in promise) Error: network access is not allowed: run qjs with --allow-net
```

Sockets work both ways, and the protocol — its frames, its fragments, its pings
— is handled underneath, so what a script sees is what the other end said:

```js
serve({port: 8080}, (request) => {
  const {socket, response} = upgradeWebSocket(request)
  socket.onmessage = (e) => socket.send("you said " + e.data)
  return response
})
```

A program that serves is a program, so `qjs` can be the whole of a small
service:

```js
// server.js -- qjs --allow-net server.js
serve({port: 8080}, async (request) => {
  const {pathname} = new URL(request.url)
  if (pathname === "/health") return new Response("ok")
  return Response.json({path: pathname})
})
```

The bounds are there too — `--memory-limit 64m`, `--stack-size`, `--timeout 5s`,
`--no-code-generation` — and `--check` parses without running. `--inspect`,
`--inspect-wait` and `--inspect-brk` let Chrome's DevTools or VS Code attach,
as Node's do; see [Debugging](debugging.md). The prompt keeps
an unfinished line rather than refusing it, so a function can be typed over
several lines, leaves the last value in `_`, and takes a top-level `await`. On
a terminal the line is edited as it is typed -- the cursor moves by character
and by word, the arrows go through a history kept in `~/.qjs_history`, and Tab
completes globals and properties -- and Ctrl-C stops what is running without
leaving the prompt:

```
> const res = await fetch("https://example.com")
> res.status
200
```
