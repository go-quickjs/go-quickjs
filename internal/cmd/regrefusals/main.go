// Command regrefusals is the register backend's refusal inventory: it
// compiles every .js and .mjs file under the directories it is given, with
// the register form on, and counts the functions regcomp compiled and those
// it refused, by reason (docs/register-interpreter.md, "Where stage 1
// stands").
//
//	go run ./internal/cmd/regrefusals [-top 40] [-files] dir...
//
// A file of test262 is compiled as its front matter says: a module where
// its flags have module (or its name _FIXTURE), strict where they have
// onlyStrict; a file that does not compile -- a negative test -- is
// counted and left out. Every other file is a script, or a module where it
// ends in .mjs. Each directory is reported apart and then all together.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/compiler/regcomp"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// tally is a directory's count.
type tally struct {
	files, failed  int
	funcs, regCode int
	why            map[string]int
	// example is a file with a function refused for each reason.
	example map[string]string
}

func newTally() *tally {
	return &tally{why: map[string]int{}, example: map[string]string{}}
}

func main() {
	top := flag.Int("top", 40, "reasons to list for each directory")
	files := flag.Bool("files", false, "list a file with a function refused for each reason")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: regrefusals [-top n] [-files] dir...")
		os.Exit(2)
	}
	compiler.SetRegisterForm(true)
	refusals := map[*bytecode.Function]string{}
	regcomp.OnRefuse = func(fn *bytecode.Function, why string) { refusals[fn] = why }

	all := newTally()
	for _, dir := range flag.Args() {
		t := newTally()
		filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			ext := filepath.Ext(path)
			if ext != ".js" && ext != ".mjs" {
				return nil
			}
			text, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			fn, ok := compile(path, string(text), ext == ".mjs")
			t.files++
			if !ok {
				t.failed++
				return nil
			}
			walk(fn, func(f *bytecode.Function) {
				t.funcs++
				if f.Reg != nil {
					t.regCode++
					return
				}
				why, ok := refusals[f]
				if !ok {
					why = "(not given to the backend)"
					if f.TopLevel {
						why = "top-level code"
					}
				}
				t.why[why]++
				if _, ok := t.example[why]; !ok {
					t.example[why] = path
				}
			})
			clear(refusals)
			return nil
		})
		report(dir, t, *top, *files)
		all.files += t.files
		all.failed += t.failed
		all.funcs += t.funcs
		all.regCode += t.regCode
		for k, n := range t.why {
			all.why[k] += n
			if _, ok := all.example[k]; !ok {
				all.example[k] = t.example[k]
			}
		}
	}
	if flag.NArg() > 1 {
		report("all", all, *top, *files)
	}
}

// compile compiles a file as test262's front matter, or its extension,
// says, and reports whether it compiled.
func compile(path, text string, module bool) (fn *bytecode.Function, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	strict := false
	if i := strings.Index(text, "/*---"); i >= 0 {
		if j := strings.Index(text[i:], "---*/"); j >= 0 {
			meta := text[i : i+j]
			if k := strings.Index(meta, "flags:"); k >= 0 {
				line := meta[k:]
				if e := strings.IndexByte(line, '\n'); e >= 0 {
					line = line[:e]
				}
				module = module || strings.Contains(line, "module")
				strict = strings.Contains(line, "onlyStrict")
			}
		}
	}
	module = module || strings.Contains(filepath.Base(path), "_FIXTURE")
	prog, err := parser.Parse(text, parser.Options{Module: module, Strict: strict && !module})
	if err != nil {
		return nil, false
	}
	opts := compiler.Options{Source: path, Text: text}
	if module {
		fn, _, err = compiler.CompileModule(prog, opts)
	} else {
		fn, err = compiler.Compile(prog, opts)
	}
	return fn, err == nil && fn != nil
}

// walk calls visit for fn and every function compiled inside it.
func walk(fn *bytecode.Function, visit func(*bytecode.Function)) {
	visit(fn)
	for i := range fn.Constants {
		if c := &fn.Constants[i]; c.Kind == bytecode.ConstFunction && c.Fn != nil {
			walk(c.Fn, visit)
		}
	}
}

func report(dir string, t *tally, top int, files bool) {
	fmt.Printf("%s: %d files (%d not compiled), %d functions, %d as register code (%.1f%%), %d refused\n",
		dir, t.files, t.failed, t.funcs, t.regCode, 100*float64(t.regCode)/float64(max(t.funcs, 1)), t.funcs-t.regCode)
	type row struct {
		why string
		n   int
	}
	var rows []row
	for k, n := range t.why {
		rows = append(rows, row{k, n})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].why < rows[j].why
	})
	for i, r := range rows {
		if i == top {
			fmt.Printf("  ... %d reasons more\n", len(rows)-top)
			break
		}
		if files {
			fmt.Printf("  %8d  %s  (%s)\n", r.n, r.why, t.example[r.why])
		} else {
			fmt.Printf("  %8d  %s\n", r.n, r.why)
		}
	}
}
