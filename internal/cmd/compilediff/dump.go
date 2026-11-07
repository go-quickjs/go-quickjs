package main

import (
	"bufio"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

type options struct {
	mutants, mutmax int
	modes, file     string
}

var allModes = []string{"script", "strict", "module", "debug", "moddebug", "quirks", "eval"}

// mode is how a file is compiled.
type mode struct {
	p parser.Options
	c compiler.Options
}

func modeOf(name string) (mode, error) {
	switch name {
	case "script":
		return mode{}, nil
	case "strict":
		return mode{p: parser.Options{Strict: true}}, nil
	case "module":
		return mode{p: parser.Options{Module: true}}, nil
	case "debug":
		return mode{c: compiler.Options{Debug: true}}, nil
	case "moddebug":
		return mode{p: parser.Options{Module: true}, c: compiler.Options{Debug: true}}, nil
	case "quirks":
		return mode{p: parser.Options{NodeQuirks: true}, c: compiler.Options{NodeQuirks: true}}, nil
	case "eval":
		// The code of a direct eval, in a function with these bindings.
		return mode{
			p: parser.Options{AllowSuperProp: true, AllowNewTarget: true},
			c: compiler.Options{
				EvalScope: []bytecode.EvalBinding{
					{Name: "a", FromLocal: true, Index: 0, Mutable: true, VarScoped: true},
					{Name: "x", FromLocal: true, Index: 1, Mutable: true, TDZ: true, Lexical: true},
					{Name: "f", Index: 0, FuncSelf: true},
					{Name: "arguments", FromLocal: true, Index: 2, Mutable: true, VarScoped: true},
				},
				EvalOwnVarScope: true, EvalConfigurable: true, AllowSuperProp: true, AllowNewTarget: true,
			},
		}, nil
	}
	return mode{}, fmt.Errorf("no mode %q", name)
}

// inserts are what a mutation adds: what the lexer and the parser decide
// things by, and what is easily got wrong in literals.
var inserts = []string{
	"\n", "\r\n", "\u2028", "\u2029", "\u00a0", "\ufeff", "\u3000", "=>", ":", "/*c*/", "/*\n*/", "// x\n",
	"<!-- x\n", "\n--> x\n", "async", "await", "yield", "let", "static", "of", "get", "set",
	"`", "${", "}", "{", "(", ")", "[", "]", "\\u0061", "\\u{61}", "a\\u0062", "\\", "010", "08.5", "09", "1_0", ".5", "5.",
	"0.1e1", "1n", "0n", "1e400", "123456789012345", "1234567890123456", "12345.6789012345", "0x1F", "0b11", "0o7", "1e-5",
	"/", "/=", "/x/g", "x", "é", "𝒳", "'\\\n'", "'\\u{10FFFF}'", "'\\uD800'", "\"\\x41\"", "`a\\\r\nb`", "`\r\n`", "`\\u`", "`\\01`",
	"function", "class", "new", "this", "super", "arguments", "eval", "with(a)", "?.", "??", "...", "=", ",", ";", "#x",
	"try{}catch(e){}", "var e", "var a", "let a", "function a(){}", "class a{}", "eval('x')", "break", "continue", "return", "a:",
}

func mutate(src string, r *rand.Rand) string {
	for range 1 + r.IntN(3) {
		if src == "" {
			break
		}
		p := startOfRune(src, r.IntN(len(src)+1))
		token := inserts[r.IntN(len(inserts))]
		switch r.IntN(4) {
		case 0, 1:
			src = src[:p] + token + src[p:]
		case 2:
			src = src[:p] + src[startOfRune(src, min(len(src), p+1+r.IntN(8))):]
		default:
			src = src[:p] + " " + token + " " + src[p:]
		}
	}
	return src
}

func startOfRune(s string, p int) int {
	for p < len(s) && s[p]&0xC0 == 0x80 {
		p++
	}
	return p
}

// result is a compile's output.
type result struct {
	fn   *bytecode.Function
	info any
	err  string
}

func compile(name, text string, m mode) (res result) {
	defer func() {
		if e := recover(); e != nil {
			res = result{err: fmt.Sprintf("panic: %v", e)}
		}
	}()
	prog, err := parser.Parse(text, m.p)
	if err != nil {
		return result{err: fmt.Sprintf("%T %#v", err, err)}
	}
	opts := m.c
	opts.Source, opts.Text = name, text
	if m.p.Module {
		fn, info, err := compiler.CompileModule(prog, opts)
		if err != nil {
			return result{err: fmt.Sprintf("%T %#v", err, err)}
		}
		return result{fn: fn, info: info}
	}
	fn, err := compiler.Compile(prog, opts)
	if err != nil {
		return result{err: fmt.Sprintf("%T %#v", err, err)}
	}
	return result{fn: fn}
}

// writer writes a compile's output as text.
type writer struct {
	w    io.Writer
	seen map[uintptr]int
}

var (
	functionType = reflect.TypeFor[bytecode.Function]()
	scriptType   = reflect.TypeFor[bytecode.Script]()
)

func (d *writer) result(res result, text string) {
	if res.fn == nil {
		fmt.Fprintln(d.w, "error", res.err)
		return
	}
	d.function("main", res.fn)
	if res.info != nil {
		fmt.Fprint(d.w, "module ")
		d.value(reflect.ValueOf(res.info))
		fmt.Fprintln(d.w)
	}
	d.lineTable(res.fn.Script, text)
}

// function writes all of fn's fields, each on a line, and an instruction
// or a constant a line each, and then the functions it holds.
func (d *writer) function(path string, fn *bytecode.Function) {
	fmt.Fprintf(d.w, "function %s %s\n", path, fn.Name)
	v := reflect.ValueOf(fn).Elem()
	for i := range v.NumField() {
		f := functionType.Field(i)
		switch f.Name {
		case "Code":
			for pc, in := range fn.Code {
				pos, ok := fn.PosAt(uint32(pc))
				fmt.Fprintf(d.w, "%6d %s %d %d @%d %v\n", pc, in.Op, in.A, in.B, pos, ok)
			}
			continue
		case "Constants":
			for i, c := range fn.Constants {
				fmt.Fprintf(d.w, "const %d %d %x %q %q %v\n", i, c.Kind, c.Num, c.Str, c.Flags, c.Fn != nil)
			}
			continue
		}
		fmt.Fprint(d.w, f.Name, ": ")
		d.value(v.Field(i))
		fmt.Fprintln(d.w)
	}
	n := 0
	for _, c := range fn.Constants {
		if c.Fn != nil {
			d.function(fmt.Sprintf("%s/%d", path, n), c.Fn)
			n++
		}
	}
}

// lineTable writes what the script answers for the start of each line and
// for a sample of offsets: all that its table decides. Offsets past the
// text's end and before its start answer too.
func (d *writer) lineTable(s *bytecode.Script, text string) {
	if s == nil {
		return
	}
	fmt.Fprint(d.w, "lines ")
	ask := func(p int) {
		l, c := s.Position(int32(p))
		fmt.Fprint(d.w, l, ":", c, " ")
	}
	ask(-1)
	ask(0)
	for p := 0; p < len(text); {
		i := strings.IndexByte(text[p:], '\n')
		if i < 0 {
			break
		}
		p += i + 1
		ask(p)
	}
	for p, step := 0, max(1, len(text)/97); p <= len(text)+1 && len(text) < 1<<20; p += step {
		ask(p)
	}
	ask(len(text))
	ask(len(text) + 1)
	fmt.Fprintln(d.w)
}

func (d *writer) value(v reflect.Value) {
	switch v.Kind() {
	case reflect.Bool:
		fmt.Fprint(d.w, v.Bool(), " ")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		fmt.Fprint(d.w, v.Int(), " ")
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		fmt.Fprint(d.w, v.Uint(), " ")
	case reflect.Float32, reflect.Float64:
		fmt.Fprintf(d.w, "%x ", v.Float())
	case reflect.String:
		fmt.Fprintf(d.w, "%q ", v.String())
	case reflect.Slice:
		if v.IsNil() {
			fmt.Fprint(d.w, "nil ")
			return
		}
		fmt.Fprintf(d.w, "[%d ", v.Len())
		for i := range v.Len() {
			d.value(v.Index(i))
		}
		fmt.Fprint(d.w, "] ")
	case reflect.Array:
		fmt.Fprint(d.w, "[ ")
		for i := range v.Len() {
			d.value(v.Index(i))
		}
		fmt.Fprint(d.w, "] ")
	case reflect.Map:
		keys := make([]string, 0, v.Len())
		for it := v.MapRange(); it.Next(); {
			var b strings.Builder
			e := writer{w: &b, seen: d.seen}
			e.value(it.Key())
			e.value(it.Value())
			keys = append(keys, b.String())
		}
		sort.Strings(keys)
		fmt.Fprint(d.w, keys, " ")
	case reflect.Pointer:
		switch {
		case v.IsNil():
			fmt.Fprint(d.w, "nil ")
		case v.Type().Elem() == functionType:
			// Each is written by function, at its own place.
			fmt.Fprint(d.w, "fn ")
		case v.Type().Elem() == scriptType:
			// All the script's fields but its text, which is what was
			// compiled, and its table, which lineTable asks.
			s := v.Elem()
			for _, name := range []string{"Name", "EvalOrigin", "Referrer", "Comments", "lineOffset", "columnOffset"} {
				d.value(s.FieldByName(name))
			}
		default:
			if id, ok := d.seen[v.Pointer()]; ok {
				fmt.Fprintf(d.w, "ref%d ", id)
				return
			}
			d.seen[v.Pointer()] = len(d.seen)
			fmt.Fprint(d.w, "& ")
			d.value(v.Elem())
		}
	case reflect.UnsafePointer:
		fmt.Fprint(d.w, v.IsNil(), " ")
	case reflect.Interface:
		if v.IsNil() {
			fmt.Fprint(d.w, "nil ")
			return
		}
		fmt.Fprint(d.w, v.Elem().Type(), " ")
		d.value(v.Elem())
	case reflect.Struct:
		fmt.Fprint(d.w, "{")
		t := v.Type()
		for i := range t.NumField() {
			fmt.Fprint(d.w, t.Field(i).Name, ":")
			d.value(v.Field(i))
		}
		fmt.Fprint(d.w, "} ")
	default:
		fmt.Fprint(d.w, v.Kind(), " ")
	}
}

// source is a file to compile: name is what dump calls it, its root's last
// element and its path under that.
type source struct{ name, path string }

func sources(roots []string) ([]source, error) {
	var files []source
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(p, ".js") {
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return err
				}
				base := filepath.Base(root)
				files = append(files, source{name: filepath.ToSlash(filepath.Join(base, rel)), path: p})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}

// summary is a file's line in a dump.
type summary struct {
	hash                  uint64
	made, rejected, crash int
	bytes                 int64
}

// compileFile compiles a file in each mode with its mutations, writing the
// output of each to w. A compile's output is written after the next compile
// is made (see the package's comment).
func compileFile(w io.Writer, f source, modes []string, o options, verbose bool) (s summary, err error) {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return s, err
	}
	text := string(b)
	var prev result
	var prevText string
	var prevHead string
	flush := func() {
		if prevHead != "" {
			fmt.Fprintln(w, prevHead)
			d := writer{w: w, seen: map[uintptr]int{}}
			d.result(prev, prevText)
		}
	}
	seed := fnv.New64a()
	seed.Write([]byte(f.name))
	for _, name := range modes {
		m, err := modeOf(name)
		if err != nil {
			return s, err
		}
		variants := 1
		if len(text) <= o.mutmax {
			variants += o.mutants
		}
		r := rand.New(rand.NewPCG(seed.Sum64(), uint64(len(name))))
		for v := range variants {
			src := text
			if v > 0 {
				src = mutate(text, r)
			}
			res := compile(f.name, src, m)
			flush()
			s.bytes += int64(len(src))
			switch {
			case res.fn != nil:
				s.made++
			case strings.HasPrefix(res.err, "panic"):
				s.crash++
			default:
				s.rejected++
			}
			prev, prevText = res, src
			prevHead = fmt.Sprintf("=== %s mode %s variant %d", f.name, name, v)
			if verbose && v > 0 {
				prevHead += fmt.Sprintf(" source %q", src)
			}
		}
	}
	flush()
	return s, nil
}

func modeList(o options) []string { return strings.Split(o.modes, ",") }

// dumpAll writes a line for each file, its name and its output's hash and
// counts, and to log how much was compiled.
func dumpAll(out, log io.Writer, roots []string, o options) error {
	files, err := sources(roots)
	if err != nil {
		return err
	}
	modes := modeList(o)
	results := make([]summary, len(files))
	errs := make([]error, len(files))
	next := make(chan int)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				h := fnv.New64a()
				w := bufio.NewWriterSize(h, 1<<16)
				s, err := compileFile(w, files[i], modes, o, false)
				w.Flush()
				s.hash = h.Sum64()
				results[i], errs[i] = s, err
			}
		}()
	}
	for i := range files {
		next <- i
	}
	close(next)
	wg.Wait()
	w := bufio.NewWriter(out)
	defer w.Flush()
	var total summary
	for i, s := range results {
		if errs[i] != nil {
			return errs[i]
		}
		fmt.Fprintf(w, "%s\t%016x\t%d\t%d\t%d\n", files[i].name, s.hash, s.made, s.rejected, s.crash)
		total.made += s.made
		total.rejected += s.rejected
		total.crash += s.crash
		total.bytes += s.bytes
	}
	fmt.Fprintf(log, "%d files, %d compiled, %d rejected, %d panicked, %d MB of source\n",
		len(files), total.made, total.rejected, total.crash, total.bytes>>20)
	return nil
}

// showFile writes all of a file's output, as text.
func showFile(out io.Writer, roots []string, o options) error {
	files, err := sources(roots)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(out, 1<<20)
	defer w.Flush()
	for _, f := range files {
		if f.name == o.file {
			_, err := compileFile(w, f, modeList(o), o, true)
			return err
		}
	}
	return fmt.Errorf("no file %s", o.file)
}
