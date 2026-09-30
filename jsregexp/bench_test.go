package jsregexp_test

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/jsregexp"
)

// The benchmarks compare this package with Go's regexp on work both can do:
// the same pattern, meaning the same thing in both syntaxes, over the same
// input. Each case runs once per engine, as engine=jsregexp, engine=regexp and
// engine=jsregexp-utf8 -- a pattern from CompileUTF8, which matches a subject's
// UTF-8 where it is -- so that benchstat can set them side by side:
//
//	go test ./jsregexp -run '^$' -bench Compare -count 10 > bench.txt
//	benchstat -col /engine bench.txt
//
// TestCompareCasesAgree checks that both engines give the same answer to
// every case, so that the timings compare like with like.

// logText is some 16 KB of log lines: what a program most often searches.
var logText = func() string {
	levels := []string{"INFO", "WARN", "INFO", "DEBUG", "ERROR", "INFO"}
	users := []string{"alice", "bob", "carol", "dave", "erin"}
	var b strings.Builder
	for i := 0; b.Len() < 16<<10; i++ {
		fmt.Fprintf(&b, "2026-09-%02d 12:%02d:%02d %s user=%s@example.com processing request %d, took %d ms\n",
			1+i%28, i%60, (i*7)%60, levels[i%len(levels)], users[i%len(users)], 1000+i*37, 3+i%250)
	}
	// One line that only the literal searches look for, near the end.
	b.WriteString("2026-09-30 23:59:59 FATAL needle found in haystack\n")
	return b.String()
}()

// unicodeText is the log with a word outside ASCII in every line, which a
// UTF-16 engine has to convert the text for.
var unicodeText = strings.ReplaceAll(logText, "processing", "traitement déjà")

type compareCase struct {
	name     string
	js       string // the pattern for jsregexp, whose flags are jsFlags
	jsFlags  string
	re       string // the same pattern in Go's syntax
	op       string // match, find, findall, submatch, replace or split
	input    string
	replace  string
	findAllN int
}

var compareCases = []compareCase{
	{name: "LiteralFound", js: `needle`, re: `needle`, op: "find", input: logText},
	{name: "LiteralMissing", js: `zebra`, re: `zebra`, op: "match", input: logText},
	{name: "LiteralMissingUnicode", js: `zebra`, re: `zebra`, op: "match", input: unicodeText},
	{name: "Anchored", js: `^\d{4}-\d{2}-\d{2}$`, re: `^\d{4}-\d{2}-\d{2}$`, op: "match", input: "2026-09-30"},
	// The log begins with a date, but not only a date: the match fails, at
	// the start, which is the only place it could begin.
	{name: "AnchoredMissing", js: `^\d{4}-\d{2}-\d{2}$`, re: `^\d{4}-\d{2}-\d{2}$`, op: "match", input: logText},
	{name: "Digits", js: `\d+`, re: `\d+`, op: "findall", input: logText, findAllN: -1},
	{name: "Words", js: `\b\w+ing\b`, re: `\b\w+ing\b`, op: "findall", input: logText, findAllN: -1},
	{name: "Alternation", js: `WARN|ERROR|FATAL`, re: `WARN|ERROR|FATAL`, op: "findall", input: logText, findAllN: -1},
	{name: "CaseInsensitive", js: `error`, jsFlags: "i", re: `(?i)error`, op: "findall", input: logText, findAllN: -1},
	{name: "Captures", js: `(\w+)@(\w+)\.com`, re: `(\w+)@(\w+)\.com`, op: "submatch", input: logText},
	{name: "Email", js: `[a-z]+@[a-z]+\.[a-z]{2,}`, re: `[a-z]+@[a-z]+\.[a-z]{2,}`, op: "findall", input: logText, findAllN: -1},
	{name: "ReplaceSpaces", js: `\s+`, re: `\s+`, op: "replace", input: logText, replace: " "},
	{name: "Split", js: `,\s*`, re: `,\s*`, op: "split", input: logText},
	{name: "LiteralFoundUnicode", js: `needle`, re: `needle`, op: "find", input: unicodeText},
	{name: "DigitsUnicode", js: `\d+`, re: `\d+`, op: "findall", input: unicodeText, findAllN: -1},
	{name: "CapturesUnicode", js: `(\w+)@(\w+)\.com`, re: `(\w+)@(\w+)\.com`, op: "submatch", input: unicodeText},
	{name: "ReplaceSpacesUnicode", js: `\s+`, re: `\s+`, op: "replace", input: unicodeText, replace: " "},
}

// compareOp is one case's work on one engine, returning what it found so
// that the two engines' answers can be compared.
type compareOp func() []string

func jsOp(b testing.TB, c compareCase) compareOp {
	return jsOpWith(b, c, jsregexp.Compile)
}

func jsUTF8Op(b testing.TB, c compareCase) compareOp {
	return jsOpWith(b, c, jsregexp.CompileUTF8)
}

func jsOpWith(b testing.TB, c compareCase, compile func(pattern, flags string) (*jsregexp.Regexp, error)) compareOp {
	re, err := compile(c.js, c.jsFlags)
	if err != nil {
		b.Fatal(err)
	}
	must := func(v []string, err error) []string {
		if err != nil {
			b.Fatal(err)
		}
		return v
	}
	switch c.op {
	case "match":
		return func() []string {
			ok, err := re.MatchString(c.input)
			return must([]string{fmt.Sprint(ok)}, err)
		}
	case "find":
		return func() []string {
			s, err := re.FindString(c.input)
			return must([]string{s}, err)
		}
	case "findall":
		return func() []string { return must(re.FindAllString(c.input, c.findAllN)) }
	case "submatch":
		return func() []string { return must(re.FindStringSubmatch(c.input)) }
	case "replace":
		return func() []string {
			s, err := re.ReplaceAllLiteralString(c.input, c.replace)
			return must([]string{s}, err)
		}
	case "split":
		return func() []string { return must(re.Split(c.input, -1)) }
	}
	b.Fatalf("unknown op %q", c.op)
	return nil
}

func goOp(b testing.TB, c compareCase) compareOp {
	re := regexp.MustCompile(c.re)
	switch c.op {
	case "match":
		return func() []string { return []string{fmt.Sprint(re.MatchString(c.input))} }
	case "find":
		return func() []string { return []string{re.FindString(c.input)} }
	case "findall":
		return func() []string { return re.FindAllString(c.input, c.findAllN) }
	case "submatch":
		return func() []string { return re.FindStringSubmatch(c.input) }
	case "replace":
		return func() []string { return []string{re.ReplaceAllLiteralString(c.input, c.replace)} }
	case "split":
		return func() []string { return re.Split(c.input, -1) }
	}
	b.Fatalf("unknown op %q", c.op)
	return nil
}

// TestCompareCasesAgree pins that each benchmark case asks both engines the
// same question: they must give the same answer for the timings to mean
// anything.
func TestCompareCasesAgree(t *testing.T) {
	for _, c := range compareCases {
		want := goOp(t, c)()
		if got := jsOp(t, c)(); !slices.Equal(got, want) {
			t.Errorf("%s: jsregexp and regexp disagree: %d results against %d", c.name, len(got), len(want))
		}
		if got := jsUTF8Op(t, c)(); !slices.Equal(got, want) {
			t.Errorf("%s: jsregexp's UTF-8 mode and regexp disagree: %d results against %d", c.name, len(got), len(want))
		}
	}
}

func BenchmarkCompare(b *testing.B) {
	for _, c := range compareCases {
		engines := []struct {
			name string
			op   func(testing.TB, compareCase) compareOp
		}{{"jsregexp", jsOp}, {"jsregexp-utf8", jsUTF8Op}, {"regexp", goOp}}
		for _, e := range engines {
			b.Run(c.name+"/engine="+e.name, func(b *testing.B) {
				op := e.op(b, c)
				b.SetBytes(int64(len(c.input)))
				b.ReportAllocs()
				for b.Loop() {
					op()
				}
			})
		}
	}
}
