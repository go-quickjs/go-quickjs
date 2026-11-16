package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
)

// The fold command classifies what the compiler's folds change
// (internal/fold): it compiles every file at a revision before
// them, and here with each of fold's kinds alone (QJS_FOLD), with none,
// and with all, a line a function (dump -perfunc), and says
//
//   - which functions the compile with no folds makes differently from the
//     revision: none should, as anything is then not a fold;
//   - for each function the folds change, which kinds change it alone, or
//     that only kinds together do;
//   - which functions the folds leave with no register code, or whose
//     register compile panics, where they had code, and which rejected
//     programs' errors they change: none should;
//
// counted by corpus, the first element of each file's name.
//
//	go run ./internal/cmd/compilediff fold -base origin/main /path/to/octane /path/to/test262/test
func foldReport(args []string, w io.Writer) (same bool, err error) {
	fs := flag.NewFlagSet("compilediff fold", flag.ExitOnError)
	var o options
	var base string
	fs.StringVar(&base, "base", "", "the revision before the folds")
	fs.IntVar(&o.mutants, "mutants", 0, "also compile this many mutations of each file")
	fs.IntVar(&o.mutmax, "mutmax", 64<<10, "the largest file to mutate")
	fs.StringVar(&o.modes, "modes", strings.Join(allModes, ","), "the modes to compile in")
	fs.Parse(args)
	roots := fs.Args()
	if base == "" || len(roots) == 0 {
		return false, errors.New("fold takes -base REV and directories")
	}
	dir, err := os.MkdirTemp("", "compilediff")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)
	tool, err := buildBase(base, dir)
	if err != nil {
		return false, err
	}
	self, err := os.Executable()
	if err != nil {
		return false, err
	}
	dumpArgs := append([]string{"dump", "-perfunc"}, o.flags()...)
	dumpArgs = append(dumpArgs, roots...)
	run := func(tool, kinds string) (map[string]funcLine, error) {
		fmt.Fprintf(os.Stderr, "compiling with %s\n", kinds)
		cmd := exec.Command(tool, dumpArgs...)
		cmd.Env = slices.DeleteFunc(os.Environ(), func(e string) bool { return strings.HasPrefix(e, "QJS_FOLD=") })
		if kinds != "all" && kinds != "base" {
			cmd.Env = append(cmd.Env, "QJS_FOLD="+kinds)
		}
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", tool, err)
		}
		return parseFuncLines(out), nil
	}
	before, err := run(tool, "base")
	if err != nil {
		return false, err
	}
	none, err := run(self, "none")
	if err != nil {
		return false, err
	}
	all, err := run(self, "all")
	if err != nil {
		return false, err
	}
	kinds := []string{"unary", "numeric", "strings", "condition", "deadbranch", "unreachable"}
	alone := map[string]map[string]funcLine{}
	for _, k := range kinds {
		if alone[k], err = run(self, k); err != nil {
			return false, err
		}
	}

	same = true
	var bugs []string
	for key, b := range before {
		if n, ok := none[key]; !ok || n != b {
			bugs = append(bugs, key)
		}
	}
	for key := range none {
		if _, ok := before[key]; !ok {
			bugs = append(bugs, key)
		}
	}
	sort.Strings(bugs)
	if len(bugs) > 0 {
		same = false
		fmt.Fprintf(w, "%d functions differ with no folds -- not folds:\n", len(bugs))
		for _, k := range bugs[:min(len(bugs), 20)] {
			fmt.Fprintln(w, "  ", k)
		}
	}

	type counts struct {
		funcs, changed, combined, lostReg int
		kind                              map[string]int
	}
	byCorpus := map[string]*counts{}
	var lost []string
	for key, a := range all {
		corpus := key[:strings.IndexAny(key, "/\t")]
		c := byCorpus[corpus]
		if c == nil {
			c = &counts{kind: map[string]int{}}
			byCorpus[corpus] = c
		}
		c.funcs++
		n, ok := none[key]
		if ok && n.hash == a.hash {
			continue
		}
		c.changed++
		if strings.HasSuffix(key, "\terror") {
			// A program rejected otherwise: no fold is that.
			lost = append(lost, key)
		}
		if ok && n.reg == "ok" && a.reg != "ok" {
			c.lostReg++
			lost = append(lost, key+" "+a.reg)
		}
		matched := false
		for _, k := range kinds {
			if s, ok := alone[k][key]; ok && n.hash != s.hash {
				c.kind[k]++
				matched = true
			}
		}
		if !matched {
			c.combined++
		}
	}
	if len(lost) > 0 {
		same = false
		sort.Strings(lost)
		fmt.Fprintf(w, "%d functions lose their register code, or rejected programs their error, with the folds:\n", len(lost))
		for _, k := range lost[:min(len(lost), 20)] {
			fmt.Fprintln(w, "  ", k)
		}
	}
	names := make([]string, 0, len(byCorpus))
	for k := range byCorpus {
		names = append(names, k)
	}
	sort.Strings(names)
	fmt.Fprintf(w, "%-10s %9s %8s", "corpus", "functions", "changed")
	for _, k := range kinds {
		fmt.Fprintf(w, " %11s", k)
	}
	fmt.Fprintf(w, " %9s\n", "together")
	for _, name := range names {
		c := byCorpus[name]
		fmt.Fprintf(w, "%-10s %9d %8d", name, c.funcs, c.changed)
		for _, k := range kinds {
			fmt.Fprintf(w, " %11d", c.kind[k])
		}
		fmt.Fprintf(w, " %9d\n", c.combined)
	}
	fmt.Fprintln(w, "functions count each compile's, in each mode; a function changed by")
	fmt.Fprintln(w, "two kinds alone is counted under both, and one that only kinds")
	fmt.Fprintln(w, "together change under together.")
	return same, nil
}

// funcLine is a function's line in a dump -perfunc: its output's hash, and
// whether it has register code (regState).
type funcLine struct{ hash, reg string }

// parseFuncLines reads a dump -perfunc, by file, compile and function.
func parseFuncLines(b []byte) map[string]funcLine {
	m := map[string]funcLine{}
	for _, line := range bytes.Split(b, []byte("\n")) {
		f := strings.Split(string(line), "\t")
		if len(f) != 5 {
			continue
		}
		m[f[0]+"\t"+f[1]+"\t"+f[2]] = funcLine{hash: f[3], reg: f[4]}
	}
	return m
}

// regState is what a function's output says of its register code: "ok"
// where it has some, "none", "nil" or "panic" where it has none (reg),
// "-" for the output of no function.
func regState(text []byte) string {
	i := bytes.Index(text, []byte("\nReg: "))
	if i < 0 {
		return "-"
	}
	rest := text[i+len("\nReg: "):]
	for _, s := range []string{"none", "nil", "panic"} {
		if bytes.HasPrefix(rest, []byte(s)) {
			return s
		}
	}
	return "ok"
}
