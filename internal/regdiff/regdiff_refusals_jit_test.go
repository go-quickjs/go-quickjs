//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package regdiff

import (
	"cmp"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/parser"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// TestJITRefusals counts why the JIT would not compile the functions of
// the corpora from their register code, as far as their code alone says
// (vm.JITRefusals), kind by kind, and how many of each the stack form's
// JIT takes: the list of the register form's refusals, which only ever
// shrinks (docs/register-interpreter.md, Stage 3). The corpora are the
// micro-benchmarks, and the V8 suite, Octane, Are We Fast Yet, the
// bundles and test262's tests where QUICKJS_V8_DIR, QUICKJS_OCTANE_DIR,
// QUICKJS_AWFY_DIR, QUICKJS_BUNDLE_DIR and TEST262_DIR name their
// checkouts. A function counts once, in each corpus: a program's
// top-level code is a function too, a script's or a module's.
func TestJITRefusals(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the corpora")
	}
	regDiffAlone.Lock()
	defer regDiffAlone.Unlock()
	compiler.SetRegisterForm(true)
	defer compiler.SetRegisterForm(bytecode.RegDefault())
	type corpus struct {
		name  string
		files []string
		src   string
	}
	var corpora []corpus
	micro, _ := regDiffMicro(t)
	corpora = append(corpora, corpus{name: "micro", src: micro})
	dir := func(name, env, sub string, keep func(path string) bool) {
		root := os.Getenv(env)
		if root == "" {
			t.Logf("%s: %s names no checkout", name, env)
			return
		}
		var files []string
		filepath.WalkDir(filepath.Join(root, sub), func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(path, ".js") && keep(path) {
				files = append(files, path)
			}
			return nil
		})
		slices.Sort(files)
		corpora = append(corpora, corpus{name: name, files: files})
	}
	all := func(string) bool { return true }
	dir("v8", "QUICKJS_V8_DIR", "", func(p string) bool { return !strings.HasSuffix(p, "run.js") })
	dir("octane", "QUICKJS_OCTANE_DIR", "", func(p string) bool { return !strings.HasSuffix(p, "run.js") })
	dir("awfy", "QUICKJS_AWFY_DIR", "", all)
	dir("bundles", "QUICKJS_BUNDLE_DIR", "", all)
	dir("test262", "TEST262_DIR", "test", func(p string) bool { return !strings.Contains(p, "_FIXTURE") })
	var total [2]map[string]int
	total[0], total[1] = map[string]int{}, map[string]int{}
	var out strings.Builder
	for _, c := range corpora {
		refused, taken := map[string]int{}, map[string]int{}
		examples := map[string][]string{}
		var functions, notCompiled int
		var mu sync.Mutex
		count := func(top *bytecode.Function) {
			var walk func(fn *bytecode.Function)
			walk = func(fn *bytecode.Function) {
				reg, stack := vm.JITRefusals(fn)
				mu.Lock()
				functions++
				if reg != "" {
					refused[reg]++
					if stack == "" {
						taken[reg]++
						if len(examples[reg]) < 3 {
							examples[reg] = append(examples[reg], refusalsName(fn))
						}
					}
				}
				mu.Unlock()
				for _, k := range fn.Constants {
					if k.Kind == bytecode.ConstFunction && k.Fn != nil {
						walk(k.Fn)
					}
				}
			}
			walk(top)
		}
		if c.src != "" {
			count(regLowerCompile(c.src))
		}
		work := make(chan string)
		var wg sync.WaitGroup
		// Two at once: a bundle's top-level code lowered and built takes
		// much of a small machine's memory.
		for range min(2, runtime.GOMAXPROCS(0)) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for path := range work {
					if os.Getenv("QUICKJS_REFUSALS_TRACE") != "" {
						fmt.Fprintln(os.Stderr, "refusals:", path)
					}
					top := refusalsCompile(path)
					if top == nil {
						mu.Lock()
						notCompiled++
						mu.Unlock()
						continue
					}
					count(top)
				}
			}()
		}
		for _, f := range c.files {
			work <- f
		}
		close(work)
		wg.Wait()
		n := 0
		for _, v := range refused {
			n += v
		}
		fmt.Fprintf(&out, "%s: %d files (%d not compiled), %d functions, %d refused from register code\n", c.name, len(c.files), notCompiled, functions, n)
		refusalsRows(&out, refused, taken)
		for k, ex := range examples {
			fmt.Fprintf(&out, "    the stack form takes, of %q: %s\n", k, strings.Join(ex, ", "))
		}
		for k, v := range refused {
			total[0][k] += v
			total[1][k] += taken[k]
		}
	}
	fmt.Fprintf(&out, "all corpora:\n")
	refusalsRows(&out, total[0], total[1])
	t.Log("\n" + out.String())
}

// refusalsName is fn's name and where it starts.
func refusalsName(fn *bytecode.Function) string {
	name := fn.Name
	if name == "" {
		name = "(anonymous)"
	}
	if fn.TopLevel {
		name = "(top level)"
	}
	line, _ := fn.PositionAt(0)
	src := ""
	if fn.Script != nil {
		src = fn.Script.Name
	}
	return fmt.Sprintf("%s %s:%d", name, src, line)
}

// refusalsRows writes the refusals, most first, each with how many of
// them the stack form's JIT takes.
func refusalsRows(out *strings.Builder, refused, taken map[string]int) {
	keys := make([]string, 0, len(refused))
	for k := range refused {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if c := cmp.Compare(refused[b], refused[a]); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	for _, k := range keys {
		fmt.Fprintf(out, "  %7d %s (the stack form takes %d)\n", refused[k], k, taken[k])
	}
}

// refusalsCompile is the file at path compiled with the register form, as
// a module where test262's metadata says it is one, strict where it says
// so, or nil where it does not compile.
func refusalsCompile(path string) *bytecode.Function {
	text, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	src := string(text)
	var popts parser.Options
	if meta, _, ok := strings.Cut(src, "---*/"); ok && strings.Contains(meta, "/*---") {
		if strings.Contains(meta, "negative:") {
			return nil
		}
		if i := strings.Index(meta, "flags:"); i >= 0 {
			flags, _, _ := strings.Cut(meta[i:], "\n")
			popts.Module = strings.Contains(flags, "module")
			popts.Strict = strings.Contains(flags, "onlyStrict")
		}
	}
	prog, err := parser.Parse(src, popts)
	if err != nil {
		return nil
	}
	top, err := compiler.Compile(prog, compiler.Options{Text: src, Source: path})
	if err != nil {
		return nil
	}
	return top
}
