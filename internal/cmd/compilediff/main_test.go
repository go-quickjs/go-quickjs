package main

import (
	"bytes"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

const script = "var g = 1;\nfunction sum(n) {\n  let s = `a\\n${n}`;\n  return () => s + g + 1.5;\n}\nsum(2);\n"

func write(t *testing.T, name, text string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func show(t *testing.T, dir string, o options) string {
	t.Helper()
	var out strings.Builder
	o.file = "root/a.js"
	if o.modes == "" {
		o.modes = strings.Join(allModes, ",")
	}
	if err := showFile(&out, []string{dir}, o); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// TestOutputIsWhole pins that the output has every field of a function,
// the instructions' positions and the line table's answers, and that a
// rejected program's error is exact.
func TestOutputIsWhole(t *testing.T) {
	got := show(t, write(t, "a.js", script), options{modes: "script,debug"})
	v := reflect.TypeFor[bytecode.Function]()
	for i := range v.NumField() {
		name := v.Field(i).Name
		if name == "Code" || name == "Constants" {
			continue
		}
		if !strings.Contains(got, "\n"+name+": ") {
			t.Errorf("no field %s", name)
		}
	}
	for _, want := range []string{
		"=== root/a.js mode script variant 0\n", "=== root/a.js mode debug variant 0\n",
		"function main/0 sum\n", "debug_stmt", " @", "const ", "lines 0:0 1:1 2:1 3:1 ",
		"Debug: & {",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	got = show(t, write(t, "a.js", "let a; let a;\n1 +\n"), options{modes: "script"})
	if want := `error *parser.Error &parser.Error{Msg:"unexpected end of input"`; !strings.Contains(got, want) {
		t.Errorf("no %q in\n%s", want, got)
	}
}

// TestDumpIsStable pins that two runs agree, that a change of a constant
// shows, and that mutations are the same from run to run.
func TestDumpIsStable(t *testing.T) {
	dump := func(dir string) []byte {
		var out bytes.Buffer
		o := options{mutants: 5, mutmax: 1 << 20, modes: strings.Join(allModes, ",")}
		if err := dumpAll(&out, io.Discard, []string{dir}, o); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	dir := write(t, "a.js", script)
	a, b := dump(dir), dump(dir)
	if len(diffDumps(a, b)) != 0 {
		t.Errorf("two runs differ:\n%s\n%s", a, b)
	}
	c := dump(write(t, "a.js", strings.Replace(script, "1.5", "2.5", 1)))
	if d := diffDumps(a, c); len(d) != 1 || d[0] != "root/a.js" {
		t.Errorf("diffDumps = %v", d)
	}
	r, s := rand.New(rand.NewPCG(1, 2)), rand.New(rand.NewPCG(1, 2))
	if x, y := mutate(script, r), mutate(script, s); x != y {
		t.Errorf("mutations differ: %q, %q", x, y)
	}
}

// TestFirstDifference pins that a difference is reported by compile,
// function and line.
func TestFirstDifference(t *testing.T) {
	a := show(t, write(t, "a.js", script), options{modes: "script"})
	b := show(t, write(t, "a.js", strings.Replace(script, "1.5", "2.5", 1)), options{modes: "script"})
	got := firstDifference(a, b)
	for _, want := range []string{"in === root/a.js mode script variant 0\n", "in function main/0 sum\n", "base:   Text: ", "change: Text: "} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	if got := firstDifference(a, a); got != "no difference\n" {
		t.Errorf("firstDifference(a, a) = %q", got)
	}
}
