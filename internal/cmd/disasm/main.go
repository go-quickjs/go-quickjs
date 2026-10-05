// Command disasm prints the bytecode go-quickjs compiles a script or a module
// to: each function's instructions, with the source line each comes from and,
// with -tree, whether the tree tier builds the function and, if not, why.
//
//	go run ./internal/cmd/disasm file.js
//	go run ./internal/cmd/disasm -tree -func global_read bench.js
//	go run ./internal/cmd/disasm -e 'let s = 0; for (const x of a) s += x'
//
// A file ending in .mjs is compiled as a module, as -m compiles any other.
// "-" reads the source from standard input.
//
// The source lines come from the table stack traces are made from, which
// records a position only at an instruction that can throw: a line is shown
// before the first of those it compiles to, so an instruction or two before
// it, a local read say, may belong to it as well.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/parser"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

func main() {
	var (
		expr   = flag.String("e", "", "disassemble this source instead of a file")
		module = flag.Bool("m", false, "compile as a module (the default for .mjs)")
		strict = flag.Bool("strict", false, "compile a script as strict code")
		quirks = flag.Bool("node-quirks", false, "compile as WithNodeQuirks does")
		only   = flag.String("func", "", "print only the functions of this name")
		tree   = flag.Bool("tree", false, "say whether the tree tier builds each function, and why not")
		src    = flag.Bool("src", true, "show the source line before the instructions it compiles to")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: disasm [flags] file.js | -e source\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	name, text, err := input(*expr, flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "disasm:", err)
		flag.Usage()
		os.Exit(2)
	}
	if strings.HasSuffix(name, ".mjs") {
		*module = true
	}
	fn, err := compile(name, text, *module, *strict, *quirks)
	if err != nil {
		fmt.Fprintln(os.Stderr, "disasm:", err)
		os.Exit(1)
	}
	p := printer{w: os.Stdout, lines: splitLines(text), only: *only, tree: *tree, src: *src}
	p.function(fn, "")
	if p.only != "" && p.printed == 0 {
		fmt.Fprintf(os.Stderr, "disasm: no function is named %q\n", p.only)
		os.Exit(1)
	}
}

// input is the source to compile and the name it goes by.
func input(expr string, args []string) (name, text string, err error) {
	switch {
	case expr != "" && len(args) > 0:
		return "", "", fmt.Errorf("give -e or a file, not both")
	case expr != "":
		return "<expr>", expr, nil
	case len(args) != 1:
		return "", "", fmt.Errorf("give one file")
	case args[0] == "-":
		b, err := io.ReadAll(os.Stdin)
		return "<stdin>", string(b), err
	}
	b, err := os.ReadFile(args[0])
	return filepath.ToSlash(args[0]), string(b), err
}

// compile compiles text as the engine does: a script as Runtime.Eval
// compiles one, a module as Runtime.EvalModule does.
func compile(name, text string, module, strict, quirks bool) (*bytecode.Function, error) {
	prog, err := parser.Parse(text, parser.Options{Module: module, Strict: strict && !module, NodeQuirks: quirks})
	if err != nil {
		return nil, err
	}
	opts := compiler.Options{Source: name, Text: text, NodeQuirks: quirks}
	if module {
		fn, _, err := compiler.CompileModule(prog, opts)
		return fn, err
	}
	return compiler.Compile(prog, opts)
}

func splitLines(text string) []string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, "\r")
	}
	return lines
}

type printer struct {
	w       io.Writer
	lines   []string
	only    string
	tree    bool
	src     bool
	printed int
}

// function prints fn and then the functions nested in it, each indented a
// level further. With -func, it prints those of the name, and what is nested
// in them, unindented.
func (p *printer) function(fn *bytecode.Function, indent string) {
	show := p.only == "" || fn.Name == p.only
	next := indent
	if show {
		if p.printed > 0 {
			fmt.Fprintln(p.w)
		}
		p.printed++
		p.header(fn, indent)
		p.code(fn, indent)
		next = indent + "    "
	}
	for _, c := range fn.Constants {
		if c.Kind == bytecode.ConstFunction && c.Fn != nil {
			if show && p.only != "" {
				// What is nested in a function asked for is shown with it.
				saved := p.only
				p.only = ""
				p.function(c.Fn, next)
				p.only = saved
				continue
			}
			p.function(c.Fn, next)
		}
	}
}

func (p *printer) header(fn *bytecode.Function, indent string) {
	var traits []string
	if k := kinds[fn.Kind]; k != "" {
		traits = append(traits, k)
	}
	if fn.Async {
		traits = append(traits, "async")
	}
	if fn.Generator {
		traits = append(traits, "generator")
	}
	if fn.IsModule {
		traits = append(traits, "module")
	} else if fn.TopLevel {
		traits = append(traits, "top level")
	}
	if fn.Strict {
		traits = append(traits, "strict")
	}
	if fn.HasDirectEval {
		traits = append(traits, "direct eval")
	}
	if fn.UsesArguments {
		traits = append(traits, "arguments")
	}
	if l := leaves[fn.Leaf]; l != "" {
		traits = append(traits, "leaf "+l)
	}
	at := ""
	if fn.Script != nil {
		if line, col := fn.Script.Position(fn.Start); line > 0 {
			at = fmt.Sprintf(" at %d:%d", line, col)
		}
	}
	fmt.Fprintf(p.w, "%sfunction %s%s (params=%d locals=%d stack=%d)", indent, fn, at, fn.ParamCount, fn.LocalCount, fn.MaxStack)
	if len(traits) > 0 {
		fmt.Fprintf(p.w, " %s", strings.Join(traits, ", "))
	}
	fmt.Fprintln(p.w)
	if p.tree {
		if built, why := vm.TreeReport(fn); built {
			fmt.Fprintf(p.w, "%s  tree: built\n", indent)
		} else {
			fmt.Fprintf(p.w, "%s  tree: not built: %s\n", indent, why)
		}
	}
	if len(fn.Locals) > 0 {
		names := make([]string, len(fn.Locals))
		for i, l := range fn.Locals {
			name := l.Name
			if name == "" {
				name = "(temp)"
			}
			names[i] = fmt.Sprintf("%d:%s", i, name)
		}
		fmt.Fprintf(p.w, "%s  locals: %s\n", indent, strings.Join(names, " "))
	}
	if len(fn.Upvalues) > 0 {
		names := make([]string, len(fn.Upvalues))
		for i, u := range fn.Upvalues {
			names[i] = fmt.Sprintf("%d:%s", i, u.Name)
		}
		fmt.Fprintf(p.w, "%s  upvalues: %s\n", indent, strings.Join(names, " "))
	}
}

func (p *printer) code(fn *bytecode.Function, indent string) {
	last := int32(-1)
	for pc := range fn.Code {
		if p.src {
			if line := fn.LineAt(uint32(pc)); line != last && line > 0 && int(line) <= len(p.lines) {
				last = line
				fmt.Fprintf(p.w, "%s        ; %d: %s\n", indent, line, strings.TrimSpace(p.lines[line-1]))
			}
		}
		fmt.Fprintf(p.w, "%s  %4d  %s\n", indent, pc, fn.FormatInstr(pc))
	}
}

var kinds = map[bytecode.FuncKind]string{
	bytecode.KindArrow:              "arrow",
	bytecode.KindMethod:             "method",
	bytecode.KindGetter:             "getter",
	bytecode.KindSetter:             "setter",
	bytecode.KindConstructor:        "constructor",
	bytecode.KindDerivedConstructor: "derived constructor",
	bytecode.KindClassFieldInit:     "field initializer",
	bytecode.KindStaticBlock:        "static block",
}

var leaves = map[bytecode.LeafKind]string{
	bytecode.LeafGetThis:       "get-this",
	bytecode.LeafGetThisLength: "get-this-length",
	bytecode.LeafGetThisIndex:  "get-this-index",
	bytecode.LeafSetThis:       "set-this",
	bytecode.LeafForward:       "forward",
	bytecode.LeafPure:          "pure",
}
