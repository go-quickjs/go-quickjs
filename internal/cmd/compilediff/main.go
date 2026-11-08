// Command compilediff shows whether a change to the front end leaves what it
// makes alone. It compiles every .js file of the directories it is given, in
// each of several modes -- a script, strict, a module, for a debugger, with
// Node's quirks, as a direct eval's code -- and hashes all of the output:
// every field of every function, its constants, each instruction's position,
// what the script's line table answers, and the exact error of what is
// rejected. A field added to bytecode.Function is in the output by itself.
//
// Against a revision, in one command, from the repository's root, which
// builds this tool's source over that revision in a temporary directory, runs
// both and prints the first difference, by file, mode, function and line:
//
//	go run ./internal/cmd/compilediff -base origin/main /path/to/scripts /path/to/test262/test
//
// -mutants N also compiles N mutations of each file (-mutmax is the largest
// file mutated), the same on both sides: tokens inserted, among them what the
// lexer and the parser decide things by, and text removed. Most are rejected,
// which is how errors, their types and their positions get compared.
//
// The steps can be run alone: dump prints a line a file, diff compares two
// such outputs, and show prints all of one file's output as text.
//
//	go run ./internal/cmd/compilediff dump -o change.txt scripts
//	git checkout base && go run ./internal/cmd/compilediff dump -o base.txt scripts
//	go run ./internal/cmd/compilediff diff base.txt change.txt
//	go run ./internal/cmd/compilediff show -file scripts/a.js -modes debug scripts
//
// Each function is written out after the next compile has been made, so that
// a value left sharing a buffer the next compile reuses shows as a difference.
//
// native compares two runs of v8bench's -jitcode, made with -jitheld, which
// holds the JIT's clocks and turns the collector off, but at a memory limit
// of 3 GiB unless GOMEMLIMIT gives one, so that a program makes the same
// compiles on two builds, and adds to each compile what arm64's
// backend makes of it, which is not run. Code that is the same but for
// addresses, its marks, its SSA, spills, and its exits' and calls'
// descriptions are compared, and the first compile that differs is printed:
//
//	go build -tags quickjs_jit -o base.exe ./internal/cmd/v8bench    # at the base
//	go build -tags quickjs_jit -o change.exe ./internal/cmd/v8bench  # at the change
//	base.exe -jit -jitheld -jitcode base.jsonl -bench awfy -dir /tmp/awfy -mode fixed -n 1
//	change.exe -jit -jitheld -jitcode change.jsonl -bench awfy -dir /tmp/awfy -mode fixed -n 1
//	go run ./internal/cmd/compilediff native base.jsonl change.jsonl
//
// Math.random is seeded anew for each run, so a program it steers, as Octane's
// Crypto and Typescript are, does not compile the same from one run to the
// next. native -any compares such runs function by function, in no order.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		switch args[0] {
		case "dump", "diff", "show", "native":
			cmd, args = args[0], args[1:]
		}
	}
	var err error
	same := true
	switch cmd {
	case "diff":
		if len(args) != 2 {
			err = errors.New("diff takes two dumps")
			break
		}
		same, err = diffFiles(args[0], args[1], os.Stdout)
	case "native":
		compare := compareNative
		if len(args) > 0 && args[0] == "-any" {
			compare, args = compareNativeAnyOrder, args[1:]
		}
		if len(args) != 2 {
			err = errors.New("native takes two -jitcode files")
			break
		}
		same, err = compare(args[0], args[1], os.Stdout)
	default:
		same, err = run(cmd, args)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "compilediff:", err)
		os.Exit(2)
	}
	if !same {
		os.Exit(1)
	}
}

func run(cmd string, args []string) (same bool, err error) {
	fs := flag.NewFlagSet("compilediff", flag.ExitOnError)
	var o options
	var base, out string
	fs.StringVar(&base, "base", "", "the revision to compare with")
	fs.StringVar(&out, "o", "", "dump: write to this file")
	fs.IntVar(&o.mutants, "mutants", 0, "also compile this many mutations of each file")
	fs.IntVar(&o.mutmax, "mutmax", 64<<10, "the largest file to mutate")
	fs.StringVar(&o.modes, "modes", strings.Join(allModes, ","), "the modes to compile in")
	fs.StringVar(&o.file, "file", "", "show: the file, as dump names it")
	fs.Parse(args)
	roots := fs.Args()
	if len(roots) == 0 {
		return false, errors.New("no directories given")
	}
	switch cmd {
	case "dump":
		w := io.Writer(os.Stdout)
		if out != "" {
			f, err := os.Create(out)
			if err != nil {
				return false, err
			}
			defer f.Close()
			w = f
		}
		return true, dumpAll(w, os.Stderr, roots, o)
	case "show":
		return true, showFile(os.Stdout, roots, o)
	}
	if base == "" {
		return false, errors.New("give -base REV, or one of dump, diff and show")
	}
	return compareWith(base, roots, o)
}

// flags are what passes o on to the other side's tool.
func (o options) flags() []string {
	return []string{"-mutants", fmt.Sprint(o.mutants), "-mutmax", fmt.Sprint(o.mutmax), "-modes", o.modes}
}

// compareWith runs this build and one of base.
func compareWith(rev string, roots []string, o options) (same bool, err error) {
	dir, err := os.MkdirTemp("", "compilediff")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)
	tool, err := buildBase(rev, dir)
	if err != nil {
		return false, err
	}
	self, err := os.Executable()
	if err != nil {
		return false, err
	}
	dumpArgs := append([]string{"dump"}, o.flags()...)
	dumpArgs = append(dumpArgs, roots...)
	fmt.Fprintln(os.Stderr, "compiling at", rev)
	a, err := output(tool, dumpArgs...)
	if err != nil {
		return false, err
	}
	fmt.Fprintln(os.Stderr, "compiling here")
	b, err := output(self, dumpArgs...)
	if err != nil {
		return false, err
	}
	diffs := diffDumps(a, b)
	if len(diffs) == 0 {
		fmt.Printf("same output: %d files\n", bytes.Count(a, []byte("\n")))
		return true, nil
	}
	fmt.Printf("%d files differ, the first %s\n", len(diffs), diffs[0])
	showArgs := append([]string{"show", "-file", diffs[0]}, o.flags()...)
	showArgs = append(showArgs, roots...)
	ta, err := output(tool, showArgs...)
	if err != nil {
		return false, err
	}
	tb, err := output(self, showArgs...)
	if err != nil {
		return false, err
	}
	fmt.Print(firstDifference(string(ta), string(tb)))
	for _, d := range diffs[1:min(len(diffs), 20)] {
		fmt.Println(d)
	}
	return false, nil
}

func output(tool string, args ...string) ([]byte, error) {
	cmd := exec.Command(tool, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", tool, err)
	}
	return out, nil
}

// buildBase builds this tool's source over rev in dir, and returns the
// program.
func buildBase(rev, dir string) (string, error) {
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w; run from the repository", err)
	}
	root := strings.TrimSpace(string(top))
	tree := filepath.Join(dir, "tree")
	if err := extract(rev, root, tree); err != nil {
		return "", err
	}
	const pkg = "internal/cmd/compilediff"
	dst := filepath.Join(tree, filepath.FromSlash(pkg))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return "", err
	}
	src, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pkg), "*.go"))
	if err != nil {
		return "", err
	}
	for _, p := range src {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dst, filepath.Base(p)), b, 0o644); err != nil {
			return "", err
		}
	}
	tool := filepath.Join(dir, "base-compilediff")
	if filepath.Ext(os.Args[0]) == ".exe" {
		tool += ".exe"
	}
	build := exec.Command("go", "build", "-o", tool, "./"+pkg)
	build.Dir, build.Stderr = tree, os.Stderr
	if err := build.Run(); err != nil {
		return "", fmt.Errorf("building at %s: %w", rev, err)
	}
	return tool, nil
}

// extract writes rev's files to dir.
func extract(rev, root, dir string) error {
	cmd := exec.Command("git", "archive", "--format=tar", rev)
	cmd.Dir, cmd.Stderr = root, os.Stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	r := tar.NewReader(bufio.NewReader(pipe))
	for {
		h, err := r.Next()
		if err == io.EOF {
			// The padding after the archive's end, which git would
			// otherwise block on writing.
			if _, err := io.Copy(io.Discard, pipe); err != nil {
				return err
			}
			break
		}
		if err != nil {
			return err
		}
		p := filepath.Join(dir, filepath.FromSlash(h.Name))
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(p, 0o755)
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			var f *os.File
			if f, err = os.Create(p); err != nil {
				return err
			}
			_, err = io.Copy(f, r)
			f.Close()
		}
		if err != nil {
			return err
		}
	}
	return cmd.Wait()
}

func diffFiles(a, b string, w io.Writer) (same bool, err error) {
	x, err := os.ReadFile(a)
	if err != nil {
		return false, err
	}
	y, err := os.ReadFile(b)
	if err != nil {
		return false, err
	}
	diffs := diffDumps(x, y)
	for _, d := range diffs {
		fmt.Fprintln(w, d)
	}
	if len(diffs) > 0 {
		fmt.Fprintf(w, "%d files differ; show prints a file's output, to compare\n", len(diffs))
	}
	return len(diffs) == 0, nil
}

// diffDumps is the files whose lines differ between two dumps, or that only
// one has.
func diffDumps(a, b []byte) []string {
	parse := func(d []byte) ([]string, map[string]string) {
		var order []string
		m := map[string]string{}
		for _, line := range strings.Split(string(d), "\n") {
			if i := strings.LastIndexByte(line, '\t'); i >= 0 {
				j := strings.IndexByte(line, '\t')
				order = append(order, line[:j])
				m[line[:j]] = line[j:]
			}
		}
		return order, m
	}
	order, x := parse(a)
	more, y := parse(b)
	var diffs []string
	seen := map[string]bool{}
	for _, f := range append(order, more...) {
		if !seen[f] && x[f] != y[f] {
			diffs = append(diffs, f)
		}
		seen[f] = true
	}
	return diffs
}

// firstDifference says where two shows part: the compile, the function and
// the two lines, with the lines before them that say which.
func firstDifference(a, b string) string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	var compile, function string
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return fmt.Sprintf("in %s\nin %s\nbase:   %s\nchange: %s\n", compile, function, clip(x[i]), clip(y[i]))
		}
		switch {
		case strings.HasPrefix(x[i], "=== "):
			compile, function = x[i], "no function"
		case strings.HasPrefix(x[i], "function "):
			function = x[i]
		}
	}
	if len(x) != len(y) {
		return fmt.Sprintf("in %s\nin %s\nthe output ends early, %d lines against %d\n", compile, function, len(x), len(y))
	}
	return "no difference\n"
}

func clip(s string) string {
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}
