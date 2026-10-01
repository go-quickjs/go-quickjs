# The qjs command

```
qjs script.js arg1 arg2      run a file
qjs -e 'console.log(1 + 1)'  run an expression
qjs                          read from a prompt
cat script.js | qjs -        run what arrives on standard input
```

A file that imports is run as a module without being told to; a relative
specifier is resolved against the file that named it.

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
`--no-code-generation` — and `--check` parses without running. The prompt keeps
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
