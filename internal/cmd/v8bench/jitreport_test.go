//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// Native arm64 dumps select compilediff's arm64 decoder, as the cross
// compiles do, and omit the arena's trailing alignment bytes.
func TestNativeCodeDump(t *testing.T) {
	hook, cross, annotate := vm.JITCodeHook, vm.JITCrossHook, mir.Annotate
	t.Cleanup(func() {
		vm.JITCodeHook, vm.JITCrossHook, mir.Annotate = hook, cross, annotate
	})
	path := filepath.Join(t.TempDir(), "code.jsonl")
	dump := startCodeDump(path)
	// Closed before the directory is removed, which Windows refuses
	// while the file is open.
	t.Cleanup(func() { dump.Close() })
	f, err := ssa.Build(&ir.Program{
		Code: []ir.Instruction{{Op: ir.Return, Left: ir.Literal(ir.Float(1))}},
		Maps: []ir.StateMap{{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := mir.CompileARM64(f, abi.Encoding{ValueSize: 16, StoreRef: 1})
	if err != nil {
		t.Fatal(err)
	}
	text := make([]byte, 64)
	copy(text, []byte{0xC0, 0x03, 0x5F, 0xD6}) // RET on arm64
	c.Text = 4
	vm.JITCodeHook(vm.JITFunction{Name: "f"}, text, c)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec jitCode
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	want, arch := text, ""
	if runtime.GOARCH == "arm64" {
		want, arch = text[:4], "arm64"
	}
	if rec.Arch != arch || rec.Text != 4 || !bytes.Equal(rec.Bytes, want) {
		t.Fatalf("arch %q, text %d, bytes %x; want arch %q, bytes %x", rec.Arch, rec.Text, rec.Bytes, arch, want)
	}
}

// -jitreport counts each benchmark's work from where the last report of the
// same runtime left off, and a benchmark run in another runtime from
// nothing: ssr's Render, in a runtime of its own, had been counted from
// what FirstRender's had done -- its host exits at get_prop -24, its runs
// finished in the interpreter -996, in the background -- and the functions
// of one minified line, each "(anonymous)", one from another's counts.
// Two benchmarks in one runtime, then one in another, all compiling in
// the background: every count is its own, none negative.
func TestJITReportCountsEachBenchmark(t *testing.T) {
	if !jit.Supported() {
		t.Skip("no JIT in this build")
	}
	flagWas, stateWas, stdout := *jitReportFlag, jitReport, os.Stdout
	*jitReportFlag, jitReport = true, jitReportState{}
	out, err := os.Create(filepath.Join(t.TempDir(), "report.txt"))
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = out
	t.Cleanup(func() {
		*jitReportFlag, jitReport, os.Stdout = flagWas, stateWas, stdout
		out.Close()
	})
	const work = `
		const types = []
		for (let k = 0; k < 12; k++) { const o = {}; o["p" + k] = k; if (k & 1) o.defaultProps = {k}; types.push(o) }
		function el(a, n) { if (a && a.defaultProps) return n + 1; return n }
		const fs = [function (x) { return x + 1 }, function (x) { return x * 2 }]
		function run(n) {
			let s = 0
			for (let i = 0; i < n; i++) s = (s + el(i & 1 ? "div" : types[i % 12], fs[i & 1](i))) | 0
			return s
		}`
	newRuntime := func() *quickjs.Runtime {
		rt := quickjs.New(quickjs.WithJIT(), quickjs.WithJITBackground(true))
		t.Cleanup(func() { rt.Close() })
		reportStart(runtimeOf{rt})
		if _, err := rt.Eval(work); err != nil {
			t.Fatal(err)
		}
		return rt
	}
	bench := func(rt *quickjs.Runtime, name string, n int) vm.JITStats {
		for range 20 {
			if _, err := rt.Eval(fmt.Sprintf("run(%d)", n)); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Println(name)
		runtimeOf{rt}.Report(name)
		return hostaccess.VM(rt).JITStats()
	}
	a := newRuntime()
	bench(a, "one", 5000)
	bench(a, "two", 5000)
	b := newRuntime()
	own := bench(b, "three", 500)
	if err := out.Sync(); err != nil {
		t.Fatal(err)
	}
	report, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range regexp.MustCompile(`[0-9]+`).FindAll(report, -1) {
		if v, err := strconv.ParseUint(string(n), 10, 64); err == nil && v >= 1<<62 {
			t.Errorf("a count of %d, negative, in the report:\n%s", int64(v), report)
			break
		}
	}
	_, third, _ := strings.Cut(string(report), "three\n")
	want := fmt.Sprintf("  jit: %d compiled, %d compiled again, %d entries, %d guard failures, %d exits to Go, %d finished in the interpreter\n",
		own.Compiled, own.Reoptimized, own.Entries, own.Guards, own.Hosts, own.Interpreted)
	if !strings.HasPrefix(third, want) {
		t.Errorf("the second runtime's benchmark: want its own counts\n%s\ngot\n%s", want, third)
	}
}
