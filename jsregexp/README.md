# jsregexp

ECMAScript regular expressions for Go: the patterns and flags a JavaScript
program writes, with an API shaped like Go's `regexp`.

```sh
go get github.com/go-quickjs/go-quickjs/jsregexp
```

```go
import "github.com/go-quickjs/go-quickjs/jsregexp"

re := jsregexp.MustCompile(`(?<user>\w+)@(\w+)\.com`, "i")
m, err := re.FindStringSubmatch("Write to Someone@Example.com today")
// m = ["Someone@Example.com", "Someone", "Example"]
```

It is the regular expression engine of [go-quickjs](../README.md), a
JavaScript engine in pure Go, made usable on its own. There is no cgo and no
WebAssembly, and it needs Go 1.24.

A pattern is compiled in one of two modes. `Compile` matches exactly as
JavaScript does, over UTF-16. `CompileUTF8` reads Go's UTF-8 where it is, as
Go's `regexp` does, and is what most Go code wants. The two agree except on
emoji and other characters outside the Basic Multilingual Plane, for patterns
without the `u` flag: [Which to use](#which-to-use) explains the choice.

## Why not Go's regexp

Go's `regexp` is RE2. It guarantees linear time by refusing every feature
that needs backtracking, and JavaScript requires exactly those features. A
pattern written for JavaScript often cannot be compiled by `regexp` at all:

| Pattern | Feature |
|---|---|
| `(\w+)\s+\1` | backreference |
| `(?<=\$)\d+`, `(?<!-)\d+` | lookbehind |
| `\d+(?=px)`, `\w+(?!\()` | lookahead |
| `(?<year>\d{4})-\k<year>` | named backreference |
| `\p{Script=Greek}+`, `\p{Emoji}` | Unicode properties (`u`) |
| `[\p{L}--[a-z]]`, `\p{RGI_Emoji}` | set operations, properties of strings (`v`) |
| `(?i:abc)def` | modifiers |
| `(?<x>a)\|(?<x>b)` | the same name in different alternatives |

`jsregexp` accepts these with JavaScript's meaning, down to details RE2 would
answer differently: `\s` includes the Unicode spaces, `.` stops at `\r`,
U+2028 and U+2029 as well as `\n`, and a group inside a quantifier is reset on
each repetition. The engine is the one go-quickjs runs test262 against.

When a pattern works in both, use whichever you like; the
[benchmarks](#performance) compare them.

## Flags

Flags are passed as a string, as they are written after a JavaScript literal:

| Flag | Meaning |
|---|---|
| `i` | ignore case |
| `m` | `^` and `$` match at line breaks too |
| `s` | `.` matches line breaks too |
| `u` | read the pattern as Unicode code points |
| `v` | as `u`, with set notation and properties of strings |
| `y` | sticky: match only where the search starts |
| `d` | record group positions (every method that returns them does anyway) |
| `g` | accepted, and has no effect: the method chooses one match or all |

## API

The methods follow Go's `regexp` and use its names. Only the string forms
exist; there are no `[]byte` methods.

```go
re, err := jsregexp.Compile(pattern, flags)     // JavaScript's UTF-16 matching
re := jsregexp.MustCompile(pattern, flags)
re, err := jsregexp.CompileUTF8(pattern, flags) // UTF-8 mode
re := jsregexp.MustCompileUTF8(pattern, flags)

re.MatchString(s)                   // (bool, error)
re.FindString(s)                    // (string, error)
re.FindStringIndex(s)               // ([]int, error)
re.FindStringSubmatch(s)            // ([]string, error)
re.FindStringSubmatchIndex(s)       // ([]int, error)
re.FindStringSubmatchMap(s)         // (map[string]string, error), named groups
re.FindAllString(s, n)              // and the Index, Submatch, SubmatchIndex forms
re.ReplaceAllString(s, repl)        // JavaScript's $ forms
re.ReplaceAllLiteralString(s, repl)
re.ReplaceAllStringFunc(s, func(match string) string)
re.ReplaceAllStringSubmatchFunc(s, func(groups []string) string)
re.Split(s, n)

re.NumSubexp(), re.SubexpNames(), re.SubexpIndex(name)
re.String(), re.Flags()
jsregexp.MatchString(pattern, flags, s)
jsregexp.QuoteMeta(s)
```

Where it differs from Go's `regexp`:

- **Every matching method returns an error.** See
  [Backtracking](#backtracking).
- **Replacement templates are JavaScript's**, because a pattern written for
  JavaScript usually arrives with a replacement written for it: `$&` is the
  match, `$1` to `$99` a group, `$<name>` a named group, `` $` `` and `$'` the
  text before and after, and `$$` a dollar sign. Go's `${1}` is not a
  template here.
- **`Split` leaves out what groups capture**, unlike
  `String.prototype.split`. It returns the text between matches, as Go's
  `Split` does.
- **A group that did not take part** is `""` in the string results, as in Go,
  and `-1` in the index results.

A `Regexp` is safe to share between goroutines: each match has state of its
own.

## Backtracking

Backtracking is what the features above cost. Its worst case is exponential:
`(a+)+b` against a long run of `a` would run for longer than anyone waits. A
matcher that cannot be stopped is not safe on input a program did not write,
so every match has a budget of 100 million steps. A match that exhausts it
returns `jsregexp.ErrComplexity`, and the `Regexp` stays usable for the next
match.

```go
_, err := jsregexp.MustCompile(`(a+)+b`, "").MatchString(strings.Repeat("a", 100))
// errors.Is(err, jsregexp.ErrComplexity) == true
```

`ErrComplexity` is the only error a match returns. A pattern that is invalid
fails in `Compile` instead, with an error that says what is wrong, such
as `invalid regular expression: /(/: unterminated group`.

## Text and positions

JavaScript strings are UTF-16, and a pattern's meaning is defined over UTF-16
code units: without the `u` flag, `/./` matches half of an emoji. `Compile`
keeps that meaning exactly. A Go string that is all ASCII is matched where it
is, since each of its bytes is a code unit, and any other is converted to UTF-16
for the match.

Positions are byte offsets into the string you passed in, never UTF-16
indices, so every result slices that string. A match that begins or ends inside
an emoji, which only a pattern without `u` can make, comes back with that half
of the pair encoded on its own, as [WTF-8](https://simonsapin.github.io/wtf-8/).
Go strings that came out of go-quickjs use the same encoding for lone
surrogates, so they survive the round trip.

## UTF-8 mode

`CompileUTF8` compiles a pattern to match a string's UTF-8 bytes directly,
reading one code point at a time, with no conversion. This is how Go's own
`regexp` reads text:

```go
re := jsregexp.MustCompileUTF8(`\p{L}+`, "u")
words, err := re.FindAllString("naïve café 😀 déjà vu", -1)
// ["naïve" "café" "déjà" "vu"]
```

- **With the `u` or `v` flag, results are exactly JavaScript's.** Those flags
  already read the subject by code point.
- **Without them, results differ only for characters outside the Basic
  Multilingual Plane**, such as emoji. Each is one character where
  JavaScript sees two surrogates: `/./` matches the whole emoji and `/^.$/`
  matches a lone one. A class of surrogates, such as `[\uD800-\uDFFF]`, never
  matches, because no half is ever read alone. An emoji written in the
  pattern, or spelled `\uD83D\uDE00`, still matches, but a quantifier on the
  second half alone, as in `\uD83D\uDE00+`, cannot.
- **Positions are byte offsets**, as in `Compile`, and an empty match moves the
  next search past a whole character, as Go's `FindAll` does.
- **Invalid UTF-8 reads as Go reads it.** A byte that begins no character is
  U+FFFD, one byte wide. A lone surrogate in WTF-8 is that surrogate.

## Which to use

Both modes accept the same patterns and flags, have the same methods and
report byte offsets. They differ in one thing: how a pattern without the `u`
or `v` flag counts a character outside the Basic Multilingual Plane, such as
an emoji. JavaScript, and `Compile`, count it as two UTF-16 code units;
`CompileUTF8` counts it as one character.

| | `Compile` | `CompileUTF8` |
|---|---|---|
| Pattern with `u` or `v` | JavaScript's results | the same results |
| Pattern without, on text without emoji | JavaScript's results | the same results |
| Pattern without, on text with emoji | an emoji is two characters, as in JavaScript | an emoji is one character |
| Text all in ASCII | matched where it is | matched where it is, within about 5% of `Compile`'s time |
| Text not all ASCII | converted to UTF-16 on every call | matched where it is, up to 100 times faster |

**Choose `CompileUTF8`** for most Go code: text from files, logs, requests
and users. It reads text as Go's `regexp` does, and a string not all in ASCII
costs no conversion. When the pattern has the `u` or `v` flag, or the text has
no emoji, its results are JavaScript's too, so there is nothing to give up.

**Choose `Compile`** when a pattern without the `u` flag must give the same
answer as JavaScript on text that may hold emoji. The usual case is a check
shared with a browser or Node front end:

```go
name := strings.Repeat("😀", 6)

ok, _ := jsregexp.MustCompile(`^.{1,10}$`, "").MatchString(name)     // false: 12 code units, as in JavaScript
ok, _ = jsregexp.MustCompileUTF8(`^.{1,10}$`, "").MatchString(name) // true: 6 characters
```

With `CompileUTF8` the server would accept a name the browser refuses. The
other way to keep them in step is to give the pattern the `u` flag on both
sides. Then JavaScript counts characters too, both modes answer `true`, and
`CompileUTF8` is the faster way to get that answer.

`Compile` is also the one for a pattern that works on the halves of a pair
on purpose, such as `[\uD800-\uDBFF][\uDC00-\uDFFF]` to find pairs, which never
matches in UTF-8 mode, where no half is ever read alone. And it is the one for
porting JavaScript code whose behavior must be reproduced exactly.

In short: if the pattern has `u` or `v`, or its text has no emoji, the modes
agree, so take `CompileUTF8`. Otherwise the choice comes down to whether an
emoji is one character or two.

## Performance

`BenchmarkCompare` runs the same patterns on `Compile`, `CompileUTF8` and
Go's `regexp`, over the same 16 KB of log lines. `TestCompareCasesAgree`
checks first that all three give the same answer:

```sh
go test ./jsregexp -run '^$' -bench Compare -count 10 > bench.txt
benchstat -col /engine bench.txt
```

On an AMD Ryzen 5 3600 with Go 1.27.1, the median of seven runs. The last
column is how many times faster `CompileUTF8` is than `regexp`. "Mixed" text
has a word outside ASCII on every line.

| Case | `Compile` | `CompileUTF8` | `regexp` | UTF-8 mode's speed |
|---|---:|---:|---:|---:|
| `WARN\|ERROR\|FATAL`, all matches | 34 µs | 33 µs | 855 µs | 26x |
| `error` under `i`, all matches | 71 µs | 70 µs | 305 µs | 4.4x |
| `\d+`, all matches | 164 µs | 172 µs | 494 µs | 2.9x |
| `\d+`, all matches, mixed text | 306 µs | 175 µs | 520 µs | 3.0x |
| `\s+` replaced | 216 µs | 227 µs | 634 µs | 2.8x |
| `\s+` replaced, mixed text | 428 µs | 264 µs | 693 µs | 2.6x |
| `(\w+)@(\w+)\.com`, first match | 4.1 µs | 1.2 µs | 2.0 µs | 1.7x |
| `(\w+)@(\w+)\.com`, first match, mixed text | 137 µs | 1.2 µs | 3.0 µs | 2.6x |
| `\b\w+ing\b`, all matches | 421 µs | 431 µs | 698 µs | 1.6x |
| `zebra`, not there | 314 ns | 313 ns | 479 ns | 1.5x |
| `^\d{4}-\d{2}-\d{2}$` on the log, not there | 3.1 µs | 172 ns | 233 ns | 1.4x |
| `,\s*` split | 39 µs | 39 µs | 50 µs | 1.3x |
| `^\d{4}-\d{2}-\d{2}$` on a date | 196 ns | 179 ns | 219 ns | 1.2x |
| `[a-z]+@[a-z]+\.[a-z]{2,}`, all matches | 338 µs | 349 µs | 389 µs | 1.1x |
| `needle`, found near the end | 5.5 µs | 2.6 µs | 2.8 µs | 1.1x |
| `needle`, found near the end, mixed text | 141 µs | 2.6 µs | 2.8 µs | 1.1x |

`Compile` has to convert mixed text to UTF-16 before it can match, which is
most of what it spends on the mixed cases; `CompileUTF8` matches the same
text where it is. On ASCII text `Compile` matches the bytes where they are
too, but it must read the whole string first to know that the text is ASCII:
that is the few microseconds it loses on a single early match, where
`CompileUTF8` reads no further than it matches.

A backtracking matcher that skips positions where no match can begin does
well where RE2's automaton has many states to carry, as with an alternation or
a case-insensitive pattern. Text that every match must contain is looked for
first, with the vectorized `strings.Index` RE2 also uses, so a subject without
it is turned away without being matched at all. A character repeated a fixed
number of times, as in `\d{4}`, is written out rather than counted, so a
pattern like the date's runs straight through with nothing to undo. A
pattern that begins with `^`, without the `m` flag, is tried at the start of
the string alone, where it fails on the log after ten characters.

Linear time is RE2's real advantage. If patterns come from users and
`ErrComplexity` would be a problem, prefer `regexp` whenever it can express
the pattern.

## License

MIT, as go-quickjs is: see [LICENSE](../LICENSE).
