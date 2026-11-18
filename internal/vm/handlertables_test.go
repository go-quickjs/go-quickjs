package vm

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

var updateHandlerTables = flag.Bool("update-handler-tables", false, "rewrite testdata/handlertables*.txt from the tables as they are")

// TestHandlerTables pins what the package's inits leave in the tables
// that several of them write: the register interpreter's Go handler
// tables, the typed tables, leafChecks (its lazily armed entries
// included) and, on amd64 and arm64, the native handlers' exit tables.
// Go runs a package's inits in the order of its files' names, and some of
// these inits replace or copy what an earlier one wrote: a file renamed
// past another changes the result with no build error. An entry is a
// function's name, or a closure's first source line, so that the list is
// the same whatever the files are called. A deliberate change to a table
// rewrites the lists with -update-handler-tables.
func TestHandlerTables(t *testing.T) {
	dataViewLeafOnce.Do(dataViewLeafChecks[struct{}])
	regexpLeafOnce.Do(regexpLeafChecks[struct{}])
	checkHandlerTables(t, "testdata/handlertables.txt", portableHandlerTables())
	if native := nativeHandlerTablesForTest(); native != nil {
		checkHandlerTables(t, "testdata/handlertables_native.txt", native)
	}
}

// portableHandlerTables lists the tables every architecture has.
func portableHandlerTables() []string {
	var out []string
	out = append(out, handlerTableLines("regTable", &regTable)...)
	out = append(out, handlerTableLines("regSlowTable", &regSlowTable)...)
	out = append(out, handlerTableLines("regTypedTable", &regTypedTable)...)
	out = append(out, handlerTableLines("regTypedSlowTable", &regTypedSlowTable)...)
	for op, f := range leafChecks {
		if f != nil {
			out = append(out, fmt.Sprintf("leafChecks %d %s", op, funcID(f)))
		}
	}
	for x, f := range tMath1 {
		if f != nil {
			out = append(out, fmt.Sprintf("tMath1 %d %s", x, funcID(f)))
		}
	}
	return out
}

// handlerTableLines lists a handler table's entries but rhInvalid's.
func handlerTableLines(name string, tab *[256]regHandler) []string {
	invalid := reflect.ValueOf(regHandler(rhInvalid)).Pointer()
	var out []string
	for op, h := range tab {
		if h == nil || reflect.ValueOf(h).Pointer() == invalid {
			continue
		}
		out = append(out, fmt.Sprintf("%s %d %s %s", name, op, bytecode.ROp(op), funcID(h)))
	}
	return out
}

// closureName matches a closure's name as the runtime gives it, which
// numbers a package's inits by the order of their files.
var closureName = regexp.MustCompile(`\.func\d+(\.\d+)*$`)

// funcID names a function value: its name, without the package's path, or
// for a closure its first source line.
func funcID(f any) string {
	pc := reflect.ValueOf(f).Pointer()
	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return fmt.Sprintf("unknown %#x", pc)
	}
	name := fn.Name()
	name = name[strings.LastIndex(name, "/")+1:]
	if !closureName.MatchString(name) {
		return name
	}
	file, line := fn.FileLine(pc)
	return "closure: " + sourceLine(file, line)
}

// sourceLine is a line of a file of this package, trimmed.
func sourceLine(file string, line int) string {
	b, err := os.ReadFile(filepath.Base(file))
	if err != nil {
		return fmt.Sprintf("%s:%d", filepath.Base(file), line)
	}
	lines := strings.Split(string(b), "\n")
	if line < 1 || line > len(lines) {
		return fmt.Sprintf("%s:%d", filepath.Base(file), line)
	}
	return strings.TrimSpace(lines[line-1])
}

// checkHandlerTables compares lines with the list in path, or writes it.
func checkHandlerTables(t *testing.T, path string, lines []string) {
	t.Helper()
	if *updateHandlerTables {
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var want []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		want = append(want, strings.TrimRight(s.Text(), "\r"))
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, l := range lines {
		got[l] = true
	}
	had := map[string]bool{}
	for _, l := range want {
		had[l] = true
		if !got[l] {
			t.Errorf("%s: lost %s", path, l)
		}
	}
	for _, l := range lines {
		if !had[l] {
			t.Errorf("%s: gained %s", path, l)
		}
	}
	if len(lines) != len(want) {
		t.Errorf("%s: %d entries, want %d", path, len(lines), len(want))
	}
}
