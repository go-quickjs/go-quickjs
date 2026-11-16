package v8bench

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Corpus is the source the benchmarks run, for tests that run it as a
// corpus of programs rather than time it (internal/regdiff). For "micro"
// it is microbench.js without the call of its main that ends it, then
// microbench_extra.js, and work is each test's name and the argument it
// is timed with, in microbench.js's order. For a suite's name it is the
// suite's files, read from dir (v8bench -fetch), each followed by a
// newline, in the order they load, and work is nil.
func Corpus(name, dir string) (src string, work []struct {
	Name string
	N    int
}, err error) {
	if name == "micro" {
		src, err := microSource()
		if err != nil {
			return "", nil, err
		}
		for _, w := range microWork {
			work = append(work, struct {
				Name string
				N    int
			}{w.name, w.n})
		}
		return src, work, nil
	}
	s := suites[name]
	if s == nil {
		return "", nil, fmt.Errorf("no suite %q", name)
	}
	var b strings.Builder
	for _, f := range s.files {
		text, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return "", nil, err
		}
		b.Write(text)
		b.WriteString("\n")
	}
	return b.String(), nil, nil
}
