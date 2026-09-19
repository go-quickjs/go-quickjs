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
- `internal/vm`: JavaScript runtime semantics and built-ins.
- `internal/icu`: locale, calendar, collation, segmentation, and time-zone
  behavior.
- `intldata`: the separately embedded multilingual DisplayNames data.
- `stdlib`: opt-in host functionality.
- `cmd/qjs`: the command-line host.
- `conformance`: the test262 runner. The test262 checkout is external.

The module targets Go 1.24. A `Runtime` is not safe for concurrent use; callers
give each goroutine its own runtime. Process-wide immutable data and caches,
especially under `internal/icu`, must still be safe when separate runtimes use
them concurrently.

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

- `internal/icu/tables.go` and `internal/icu/tables.bin`
- `intldata/tables.go` and `intldata/tables.bin`
- `internal/icu/cjkcollation.bin`
- `internal/regexp/unicodetables.go`

Run generators from the repository root. Use a temporary output file for
generators that write Go source to stdout so a failed run cannot truncate the
tracked file.

Core CLDR/ICU data:

```sh
generated=$(mktemp /tmp/quickjs-tables.XXXXXX.go)
go run ./internal/icu/internal/cldrgen > "$generated" && \
  mv "$generated" internal/icu/tables.go
gofmt -w internal/icu/tables.go
```

The core generator also replaces `internal/icu/tables.bin`.

Multilingual DisplayNames data:

```sh
generated=$(mktemp /tmp/quickjs-intldata.XXXXXX.go)
go run ./internal/icu/internal/cldrgen -display > "$generated" && \
  mv "$generated" intldata/tables.go
gofmt -w intldata/tables.go
```

CJK collation overlays:

```sh
go run ./internal/icu/internal/cjkgen
```

Unicode regular-expression tables require a test262 checkout:

```sh
generated=$(mktemp /tmp/quickjs-unicode.XXXXXX.go)
go run ./internal/regexp/internal/unicodegen /path/to/test262 > "$generated" && \
  mv "$generated" internal/regexp/unicodetables.go
gofmt -w internal/regexp/unicodetables.go
```

The ICU generators invoke `node`. Before regenerating, record both
`node --version` and `node -p process.versions.icu`. Do not regenerate locale
assets as part of an unrelated change: different Node/ICU releases can produce
valid but extensive semantic diffs. Regenerate both the Go index and binary
asset when the format changes, and inspect their sizes with:

```sh
wc -c internal/icu/tables.bin internal/icu/cjkcollation.bin intldata/tables.bin
```

The `.bin` files are intentional embedded assets and are marked binary in
`.gitattributes`. Do not replace them with base64 Go strings or commit raw
generator source datasets.

## Intl and data-loading invariants

- Ordinary runtime construction must not eagerly decompress Intl data.
- Hot locale records and aliases are directly indexed. Keep bounds checks on
  every generated offset before slicing embedded data.
- Cold, large datasets use indexed compressed blocks. Avoid one frame per tiny
  record because it loses cross-record compression; avoid one frame for an
  entire multilingual dataset because it creates first-use latency spikes.
- Parsing should remain feature-lazy: NumberFormat must not materialize date,
  collation, list, or relative-time tables, for example.
- `quickjs.WarmupDateTimeData()` and `quickjs.WarmupIntlData()` are the explicit
  eager paths. If a dataset is added, update the applicable warmup and verify
  that every entry was actually materialized.
- Lazy initialization must use `sync.Once` or an appropriate lock and must pass
  race testing. Do not hold a global lock while doing work that can safely be
  published per block or per locale.
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
go test -race -count=1 ./internal/icu
go test -race -count=1 . -run 'TestWarmup(Intl|DateTime)DataBeforeRuntime'
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
feature use, and verify that the explicit warmup API can absorb that cost for
servers that require predictable request latency.
