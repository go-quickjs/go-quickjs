package compiler

import (
	"os"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// typedFuncs compiles src and gives every
// function that has typed code, by name, its code compiled.
func typedFuncs(t *testing.T, src string) map[string]*bytecode.RegCode {
	t.Helper()
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fn, err := Compile(prog, Options{Source: "test", Text: src})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*bytecode.RegCode{}
	var walk func(f *bytecode.Function)
	walk = func(f *bytecode.Function) {
		if f.HasRegTyped() {
			out[f.Name] = f.RegTypedCode()
		}
		for _, k := range f.Constants {
			if k.Kind == bytecode.ConstFunction {
				walk(k.Fn)
			}
		}
	}
	walk(fn)
	return out
}

// TestAsmTypedMicroKernels compiles the asm.js module of the
// micro-benchmarks: every function of it has typed code, which the kinds
// verifier passes.
func TestAsmTypedMicroKernels(t *testing.T) {
	b, err := os.ReadFile("../v8bench/microbench_extra.js")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(string(b), "\r\n", "\n")
	fs := typedFuncs(t, src)
	for _, name := range []string{"fill", "fillF", "crcInit", "adler", "crc", "copy", "fnv", "dot", "fib", "add2",
		"callLeaf", "sw", "op0", "op1", "op2", "op3", "table"} {
		rc, ok := fs[name]
		if !ok {
			t.Errorf("%s has no typed code", name)
			continue
		}
		if rc == nil {
			t.Errorf("%s's typed code did not compile", name)
			continue
		}
		if testing.Verbose() {
			t.Logf("%s:\n%s", name, rc.String())
		}
	}
}

// TestAsmTypedZlib compiles Octane's zlib, where QUICKJS_OCTANE_DIR names
// a checkout: every function of its module has typed code.
func TestAsmTypedZlib(t *testing.T) {
	dir := os.Getenv("QUICKJS_OCTANE_DIR")
	if dir == "" {
		t.Skip("QUICKJS_OCTANE_DIR is not set")
	}
	b, err := os.ReadFile(dir + "/zlib-data.js")
	if err != nil {
		t.Fatal(err)
	}
	data := strings.ReplaceAll(string(b), "\r\n", "\n")
	i := strings.Index(data, "{zlibEval('")
	if i < 0 {
		t.Fatal("no zlibEval")
	}
	var sb strings.Builder
	for j := i + len("{zlibEval('"); j < len(data) && data[j] != '\''; j++ {
		if data[j] == '\\' {
			j++
			switch data[j] {
			case '\n':
				continue
			case 'n':
				sb.WriteByte('\n')
				continue
			}
		}
		sb.WriteByte(data[j])
	}
	fs := typedFuncs(t, sb.String())
	n, size := 0, 0
	for name, rc := range fs {
		if rc == nil {
			t.Errorf("%s's typed code did not compile", name)
			continue
		}
		n++
		size += len(rc.Code)
	}
	t.Logf("%d functions with typed code, %d instructions", n, size)
}
