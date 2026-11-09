package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/cmd/wasmbench/internal/worker"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// TestWorkloadsCompile checks that every micro-benchmark in testdata/wat is
// in the table, and that each compiles to a module the engine validates.
func TestWorkloadsCompile(t *testing.T) {
	files, err := fs.Glob(watFiles, "testdata/wat/*.wat")
	if err != nil {
		t.Fatal(err)
	}
	inTable := map[string]bool{}
	for _, w := range workloads {
		if w.file != "" {
			inTable[w.file] = true
		}
	}
	for _, f := range files {
		if !inTable[path.Base(f)] {
			t.Errorf("%s is not a workload", f)
		}
		delete(inTable, path.Base(f))
	}
	for f := range inTable {
		t.Errorf("workload file %s is missing", f)
	}

	e, err := newEngine("interp")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	for _, w := range workloads {
		if w.file == "" {
			continue
		}
		src, err := watFiles.ReadFile("testdata/wat/" + w.file)
		if err != nil {
			t.Fatal(err)
		}
		bin, err := wat.Compile(src)
		if err != nil {
			t.Errorf("%s: %v", w.file, err)
			continue
		}
		if _, err := e.Compile(context.Background(), bin); err != nil {
			t.Errorf("%s: %v", w.file, err)
		}
	}
}

// TestMicroResults runs each micro-benchmark once in the interpreter and
// checks its result.
func TestMicroResults(t *testing.T) {
	if testing.Short() {
		t.Skip("runs every micro-benchmark, a few seconds")
	}
	*cacheFlag = t.TempDir()
	for _, w := range workloads {
		if w.file == "" {
			continue
		}
		t.Run(w.name, func(t *testing.T) {
			bin, err := w.binary()
			if err != nil {
				t.Fatal(err)
			}
			e, err := newEngine("interp")
			if err != nil {
				t.Fatal(err)
			}
			req := w.request("interp", bin)
			req.Warmup, req.Reps = 0, 1
			if res := worker.Measure(e, req); res.Error != "" {
				t.Fatalf("%s: %s", res.Stage, res.Error)
			}
		})
	}
}

// TestProgramsNative builds goprog for the host and checks that each
// program workload prints what the table expects: the wasm engines are
// held to what the Go toolchain's own build computes.
func TestProgramsNative(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs goprog")
	}
	dir := t.TempDir()
	files, err := fs.Glob(goprogFiles, "testdata/goprog/*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := goprogFiles.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path.Base(f)), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module goprog\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "goprog.exe")
	build := exec.Command(goCommand(), "build", "-o", exe, ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	if msg, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build goprog: %v\n%s", err, msg)
	}
	for _, w := range workloads {
		if w.file != "" {
			continue
		}
		out, err := exec.Command(exe, w.args...).Output()
		if err != nil {
			t.Errorf("%s: %v", w.name, err)
			continue
		}
		if string(out) != w.want {
			t.Errorf("%s printed %q, want %q", w.name, out, w.want)
		}
	}
}

// TestMatrix checks the Markdown matrix: the times, the ratios and their
// geometric mean, n/a for a module an engine does not compile, and FAIL for
// a run that failed.
func TestMatrix(t *testing.T) {
	es := []engine{engines[0], engines[1], engines[3], engines[4]} // interp, jit, wazero-compiler, node
	ws := []workload{{suite: suiteMicro, name: "a"}, {suite: suiteMicro, name: "b"}}
	mk := func(w, e string, median float64) *cell {
		return &cell{Suite: suiteMicro, Workload: w, Engine: e, Samples: 1, Median: median, Compile: 1}
	}
	cells := []*cell{
		mk("a", "interp", 40), mk("a", "jit", 4), mk("a", "wazero-compiler", 2), mk("a", "node", 1),
		mk("b", "interp", 90), mk("b", "jit", 9), mk("b", "node", 1),
		{Suite: suiteMicro, Workload: "b", Engine: "wazero-compiler", Stage: "compile", Error: "gc not supported"},
	}
	var b bytes.Buffer
	printMatrix(&b, es, ws, cells)
	out := b.String()
	for _, want := range []string{
		"| workload | go-quickjs interp | go-quickjs JIT | wazero compiler | Node | interp/Node | JIT/Node | interp/wazero-c | JIT/wazero-c |",
		"| a | 40.0 | 4.00 | 2.00 | 1.00 | 40x | 4.00x | 20x | 2.00x |",
		"| b | 90.0 | 9.00 | n/a | 1.00 | 90x | 9.00x | | |",
		"| geomean | | | | | 60x | 6.00x | 20x | 2.00x |",
		"- micro/b on wazero-compiler: compile: gc not supported",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("matrix lacks %q:\n%s", want, out)
		}
	}
}
