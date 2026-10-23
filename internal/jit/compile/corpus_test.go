package compile

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// TestLowerCorpusRefusals measures the lowering's coverage of real code:
// every function of the .js files under the directory QUICKJS_JIT_REFUSALS
// names (npm's own code, Octane's, Are We Fast Yet's) is lowered as Go's
// entry lowers it (LowerSSA), and those it refuses for doing no loop's or
// array's work as native callers and inlining lower them (LowerSSAInline)
// -- but a generator or an async function, whose call makes its object and
// runs none of its body, as Go lowers it once its resumptions have made it
// hot (LowerSSAHot).
// It prints how many each takes and why the rest are refused, by the first
// reason, most common first; QUICKJS_JIT_REFUSALS_TOP is how many reasons
// to print (25). Each file is compiled as Node loads a CommonJS module,
// wrapped in a function, or else as an ECMAScript module; the wrapper's and
// the module's own code are not counted. What it prints is
// what a change to the lowering's coverage is measured by.
func TestLowerCorpusRefusals(t *testing.T) {
	dir := os.Getenv("QUICKJS_JIT_REFUSALS")
	if dir == "" {
		t.Skip("QUICKJS_JIT_REFUSALS names no directory of JavaScript files")
	}
	top := 25
	if s := os.Getenv("QUICKJS_JIT_REFUSALS_TOP"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatal(err)
		}
		top = n
	}
	standalone := map[string]int{}
	callee := map[string]int{}
	resumed := map[string]int{}
	var files, unparsed, total, lowered, calleeLowered, hotLowered int
	reason := func(err error) string {
		var rf *Refusal
		if errors.As(err, &rf) {
			return rf.Reason
		}
		return "error: " + err.Error()
	}
	var walk func(fn *bytecode.Function, count bool)
	walk = func(fn *bytecode.Function, count bool) {
		if count && !fn.TopLevel {
			total++
			_, err := LowerSSA(fn)
			switch {
			case err == nil:
				lowered++
			case HostOnly(err) && (fn.Async || fn.Generator):
				if _, err := LowerSSAHot(fn); err == nil {
					hotLowered++
				} else {
					resumed[reason(err)]++
				}
			case HostOnly(err):
				if _, err := LowerSSAInline(fn); err == nil {
					calleeLowered++
				} else {
					callee[reason(err)]++
				}
			default:
				standalone[reason(err)]++
			}
		}
		for _, c := range fn.Constants {
			if c.Kind == bytecode.ConstFunction && c.Fn != nil {
				walk(c.Fn, true)
			}
		}
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		text := string(b)
		if strings.HasPrefix(text, "#!") {
			// A hashbang comment, which only a file's start may have.
			text = "//" + text[2:]
		}
		src := "(function (exports, require, module, __filename, __dirname) {" + text + "\n})"
		prog, err := parser.Parse(src, parser.Options{})
		if err != nil {
			// An ECMAScript module.
			src = text
			prog, err = parser.Parse(src, parser.Options{Module: true})
		}
		if err != nil {
			unparsed++
			return nil
		}
		fn, err := compiler.Compile(prog, compiler.Options{Source: p, Text: src})
		if err != nil {
			unparsed++
			return nil
		}
		files++
		// The module's code, or its wrapper's, is not counted.
		for _, c := range fn.Constants {
			if c.Kind == bytecode.ConstFunction && c.Fn != nil {
				walk(c.Fn, false)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		reason string
		n      int
	}
	sorted := func(m map[string]int) []row {
		var rows []row
		for k, n := range m {
			rows = append(rows, row{k, n})
		}
		slices.SortFunc(rows, func(a, b row) int {
			if c := cmp.Compare(b.n, a.n); c != 0 {
				return c
			}
			return strings.Compare(a.reason, b.reason)
		})
		return rows[:min(top, len(rows))]
	}
	pct := func(n int) float64 { return 100 * float64(n) / float64(max(total, 1)) }
	var out strings.Builder
	refused := total - lowered - calleeLowered - hotLowered
	fmt.Fprintf(&out, "%d files (%d not parsed), %d functions\n", files, unparsed, total)
	fmt.Fprintf(&out, "%7d %5.1f%%  lowered for Go's entry\n", lowered, pct(lowered))
	fmt.Fprintf(&out, "%7d %5.1f%%  lowered for native callers and inlining (no loop or array work)\n", calleeLowered, pct(calleeLowered))
	fmt.Fprintf(&out, "%7d %5.1f%%  lowered once hot, for their resumptions (generators and async functions of no loop or array work)\n", hotLowered, pct(hotLowered))
	fmt.Fprintf(&out, "%7d %5.1f%%  refused\n", refused, pct(refused))
	fmt.Fprintf(&out, "refused, for Go's entry:\n")
	for _, r := range sorted(standalone) {
		fmt.Fprintf(&out, "%7d %5.1f%%  %s\n", r.n, pct(r.n), r.reason)
	}
	fmt.Fprintf(&out, "refused, for native callers and inlining:\n")
	for _, r := range sorted(callee) {
		fmt.Fprintf(&out, "%7d %5.1f%%  %s\n", r.n, pct(r.n), r.reason)
	}
	fmt.Fprintf(&out, "refused, once hot, for their resumptions:\n")
	for _, r := range sorted(resumed) {
		fmt.Fprintf(&out, "%7d %5.1f%%  %s\n", r.n, pct(r.n), r.reason)
	}
	t.Log("\n" + out.String())
}
