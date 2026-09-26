# AGENTS.md

This file applies to the entire repository.

## Project overview

`go-quickjs` is a pure-Go JavaScript engine modeled after QuickJS-NG. Keep it
portable: production code must not require cgo, WebAssembly, a C compiler, or
platform-specific runtime dependencies.

The main areas are:

- `quickjs.go` and other root files: the public embedding API and public
  behavior tests.
- `internal/lexer`, `internal/parser`, `internal/ast`: source processing.
- `internal/compiler`, `internal/bytecode`: compilation and VM instructions.
- `internal/vm`: JavaScript runtime semantics and built-ins. `Intl`, `Date`'s
  local time and `Temporal` are bindings over
  [go-intl](https://github.com/go-quickjs/go-intl), which holds the locale
  data, the calendars and the time zones.
- `stdlib`: opt-in host functionality.
- `cmd/qjs`: the command-line host.
- `conformance`: the test262 runner. The test262 checkout is external.

The module targets Go 1.24. A `Runtime` is not safe for concurrent use; callers
give each goroutine its own runtime. Process-wide immutable data and caches,
go-intl's included, must still be safe when separate runtimes use them
concurrently.

## Working rules

- Preserve existing behavior unless the task explicitly changes it. This is a
  language engine, so apparently small coercion, property-order, error-type,
  locale, or rounding changes can affect many unrelated tests.
- Prefer focused changes in the owning layer. Do not work around a parser or
  compiler bug in the VM, or a VM bug in the public wrapper.
- Keep the public API small and document exported identifiers.
- Run `gofmt` on every changed Go file.
- Add a regression test for every semantic bug. Prefer an exact result and
  error type over a broad smoke test.
- Do not weaken conformance expectations, add skips, or change expected
  failures merely to make a suite green. Establish the behavior in the
  applicable specification and, for compatibility work, in current engines.
- Preserve unrelated worktree changes. Do not rewrite history or use
  destructive Git commands unless explicitly requested.

## Generated files

Do not manually edit generated files or their binary companions:

- `internal/regexp/unicodetables.go`

go-intl's data is generated in go-intl, and changes there; a new go-intl is
taken with `go get` and `go mod tidy`, never by copying its files here.

Run generators from the repository root. Use a temporary output file for
generators that write Go source to stdout so a failed run cannot truncate the
tracked file.

Unicode regular-expression tables require a test262 checkout:

```sh
generated=$(mktemp /tmp/quickjs-unicode.XXXXXX.go)
go run ./internal/regexp/internal/unicodegen /path/to/test262 > "$generated" && \
  mv "$generated" internal/regexp/unicodetables.go
gofmt -w internal/regexp/unicodetables.go
```

## Intl and data-loading invariants

- Ordinary runtime construction must not read Intl data. A service reads
  go-intl's data when it is first used: `Intl` is built the first time the
  global is read, and Temporal's calendars and zones are loaded when a
  Temporal value first needs them.
- go-intl's formatters are immutable and safe to share. What the engine keeps
  per runtime -- its locale, its zone, the `Date` environment, Temporal's
  data -- lives on the `Runtime`, never in package variables.
- A difference from Node is one of go-intl's named divergences, chosen in
  `intlCompat`: standards mode takes the standard's side, `WithNodeQuirks`
  Node's. The engine adds none of its own.
- Error messages Intl and Temporal throw are V8's, word for word.
- Keep `new Date().toString()`, `new Date().toTimeString()`, and Intl time-zone
  names aligned with the selected Node/ICU data across locales, zones, seasons,
  and historical transitions.
- When Test262 disagrees with current Node and Chrome, retain a focused
  regression test documenting the observed engine behavior rather than
  silently changing the result to satisfy one stale expectation.

## Validation

Start with the smallest relevant package while iterating, then run the full
suite before handing off a substantial change:

```sh
go test ./...
git diff --check
```

For concurrency or shared-cache changes:

```sh
go test -race -count=1 -run TestIntlConcurrentRuntimes .
```

For portability-sensitive changes:

```sh
GOOS=windows GOARCH=amd64 go build ./...
GOOS=windows GOARCH=arm64 go build ./...
```

For Intl changes, also run the exact golden tests:

```sh
go test . -run '^(TestIntlFormats|TestIntlMatchesICU)$' -count=1 -v
```

If `/tmp/test262` is available, run the relevant conformance area. For Intl:

```sh
TEST262_DIR=/tmp/test262 go test ./conformance \
  -run TestConformance -count=1 -timeout 30m -v \
  -args -conformance.dir=intl402 -conformance.max-failures=20
```

For parser, compiler, VM, or built-in changes, use the narrowest applicable
`-conformance.dir` while iterating, then broaden coverage in proportion to the
change. A passing Go test command is not enough if its log reports newly
introduced conformance failures.

Performance-sensitive work should compare fresh processes, not just repeated
operations in one warmed process. Report bundle size, first-use latency,
allocations, and peak RSS. Distinguish work moved from package startup to first
feature use.
