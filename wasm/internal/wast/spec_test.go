package wast

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// specFiles returns the .wast files of the suites the environment names:
// WASM_SPEC_DIR, a WebAssembly/spec checkout (its test/core, recursively,
// and test/legacy), and WASM_THREADS_DIR, a WebAssembly/threads checkout
// (its test/core/threads).
func specFiles(t *testing.T) []string {
	spec := os.Getenv("WASM_SPEC_DIR")
	if spec == "" {
		t.Skip("set WASM_SPEC_DIR to a WebAssembly/spec checkout to run the spec suites")
	}
	var files []string
	walk := func(dir string) {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".wast") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	walk(filepath.Join(spec, "test", "core"))
	walk(filepath.Join(spec, "test", "legacy"))
	if threads := os.Getenv("WASM_THREADS_DIR"); threads != "" {
		walk(filepath.Join(threads, "test", "core", "threads"))
	}
	sort.Strings(files)
	return files
}

// expectation is what a script expects of a module.
type expectation uint8

const (
	expectValid     expectation = iota // defined, instantiated, or asserted to link or trap
	expectInvalid                      // assert_invalid
	expectMalformed                    // assert_malformed
)

// scriptModule is a module of a script with what the script expects of it.
type scriptModule struct {
	file   string
	line   int
	module *Module
	expect expectation
}

// modules lists the modules of cmds, threads' included.
func modules(file string, cmds []Command) []scriptModule {
	var ms []scriptModule
	add := func(m *Module, e expectation) {
		if m != nil {
			ms = append(ms, scriptModule{file: file, line: m.Line, module: m, expect: e})
		}
	}
	for _, c := range cmds {
		switch c := c.(type) {
		case *ModuleCommand:
			add(c.Module, expectValid)
		case *AssertUnlinkable:
			add(c.Module, expectValid)
		case *AssertUninstantiable:
			add(c.Module, expectValid)
		case *AssertInvalid:
			if !c.Custom {
				add(c.Module, expectInvalid)
			}
		case *AssertMalformed:
			if !c.Custom {
				add(c.Module, expectMalformed)
			}
		case *ThreadCommand:
			ms = append(ms, modules(file, c.Commands)...)
		}
	}
	return ms
}

// TestSpecSuites parses every script of the spec suites and compiles every
// text module in them: each module a script defines, links or instantiates
// and each module assert_invalid gives must compile, and each quoted module
// assert_malformed gives must not.
func TestSpecSuites(t *testing.T) {
	files := specFiles(t)
	var parsed, compiled, rejected, binaries, failures int
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		name, _ := filepath.Rel(filepath.Dir(filepath.Dir(file)), file)
		cmds, err := Parse(src)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			failures++
			continue
		}
		parsed++
		for _, m := range modules(name, cmds) {
			if m.module.Kind == BinaryModule {
				binaries++
				continue
			}
			_, err := m.module.Binary()
			switch {
			case m.expect == expectMalformed && err == nil:
				t.Errorf("%s:%d: malformed module compiled", name, m.line)
				failures++
			case m.expect == expectMalformed:
				rejected++
			case err != nil:
				t.Errorf("%s:%d: %v", name, m.line, err)
				failures++
			default:
				compiled++
			}
		}
	}
	t.Logf("%d of %d scripts parsed; %d text modules compiled, %d malformed text modules rejected, %d binary modules skipped, %d failures",
		parsed, len(files), compiled, rejected, binaries, failures)
}
