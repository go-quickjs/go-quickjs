package quickjs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// testSwitchesAllowed are the package variables a test may still set,
// each with why: what every runtime in the process reads, a test sets
// only where no runtime made meanwhile -- another test's, run in
// parallel -- can see it, or not at all. A test picks a mode for its own
// runtimes instead (AGENTS.md, "How a test picks a mode").
var testSwitchesAllowed = map[string]string{
	// The register interpreter's Go handler tables, which runReg's loop
	// indexes directly: a table of the runtime's would be a load more for
	// every instruction. The tests that wrap their entries to count them
	// -- the operation mixes, typed code's -- are not parallel.
	"internal/vm.regTable":          "the Go table's loop reads it directly",
	"internal/vm.regSlowTable":      "the Go table's loop reads it directly",
	"internal/vm.regTypedTable":     "the typed Go table's loop reads it directly",
	"internal/vm.nativeTyped":       "swapped with regTypedTable, in the same non-parallel test",
	"wasm/internal/engine.regTable": "the Go table's loop reads it directly",
	"internal/vm.jitWorkerPanics": "a count of the process's workers' panics, TestMain's, " +
		"which a test that makes one on purpose takes back",
	// Algorithms' thresholds, which the package's own tests lower to run
	// the large algorithms on small inputs: no runtime runs in their
	// test binaries, whose tests are not parallel.
	"internal/bignum.barrettThreshold":  "the package's own tests, no runtime",
	"internal/bignum.burnikelThreshold": "the package's own tests, no runtime",
	"internal/bignum.fftThreshold":      "the package's own tests, no runtime",
	"internal/bignum.newtonThreshold":   "the package's own tests, no runtime",
	"internal/bignum.parseSplit":        "the package's own tests, no runtime",
	"internal/bignum.toomThreshold":     "the package's own tests, no runtime",
	"internal/parser.maxFoldedString":   "the package's own tests, no runtime",
	// The WebAssembly engine's process-wide facts and a measuring host's
	// setting: the CPU's features, which the relaxed SIMD test sets to run
	// each one's code, the interpreter's and the JIT's alike; the engine's
	// memory limits, which the spec differential test lowers for both its
	// runs; the optimizing tier's phase timing (opt.Timing), which the
	// corpus test, measuring, sets with no compile in the background; and
	// the codes' ID counter, which a test takes IDs from as the engine
	// does. Their tests are not parallel.
	"wasm/internal/engine.relaxedDotSigned": "the CPU's features",
	"wasm/internal/engine.relaxedDotVNNI":   "the CPU's features",
	"wasm/internal/engine.relaxedFused":     "the CPU's features",
	"wasm/internal/jit.CPU":                 "the CPU's features",
	"wasm/internal/engine.MaxMemory32Pages": "the engine's memory limit",
	"wasm/internal/engine.MaxMemory64Pages": "the engine's memory limit",
	"wasm/internal/jit/opt.Timing":          "a measuring host's setting",
	"wasm/internal/engine.jitCodeIDs":       "an ID counter, not a switch",
}

// TestNoTestSetsAProcessSwitch fails where a test assigns a package
// variable of the module's code -- a switch, a counter, a hook -- or
// stores to one (Store, Add, Swap), which every runtime in the process
// reads: a test of a mode then changed what every other test's runtimes
// did, and such tests could not run in parallel. A test makes its
// runtimes in the mode it needs instead. An init's assignment, a
// setting of the test binary made before any test runs, is allowed; so
// are testSwitchesAllowed's, and the tools' (internal/cmd), which set
// their own flags. And internal/vm's tests make every runtime by
// newTestRuntime, which gives it the switches its test set.
func TestNoTestSetsAProcessSwitch(t *testing.T) {
	t.Parallel()
	vars := map[string]map[string]bool{} // import path within the module -> its package variables
	var tests []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != "." && (strings.HasPrefix(name, ".") || name == "testdata" || strings.HasPrefix(name, "zz") ||
				filepath.ToSlash(path) == "internal/cmd") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			tests = append(tests, path)
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		if vars[dir] == nil {
			vars[dir] = map[string]bool{}
		}
		for _, decl := range f.Decls {
			if g, ok := decl.(*ast.GenDecl); ok && g.Tok == token.VAR {
				for _, s := range g.Specs {
					for _, n := range s.(*ast.ValueSpec).Names {
						if n.Name != "_" {
							vars[dir][n.Name] = true
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	const module = "github.com/go-quickjs/go-quickjs"
	var found []string
	for _, path := range tests {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		own := vars[dir]
		if strings.HasSuffix(f.Name.Name, "_test") {
			// An external test package: its own names are not the
			// package's.
			own = nil
		}
		imports := map[string]string{}
		for _, im := range f.Imports {
			p := strings.Trim(im.Path.Value, `"`)
			name := p[strings.LastIndexByte(p, '/')+1:]
			if im.Name != nil {
				name = im.Name.Name
			}
			if p == module {
				imports[name] = "."
			} else if strings.HasPrefix(p, module+"/") {
				imports[name] = strings.TrimPrefix(p, module+"/")
			}
		}
		// root is the package variable e's expression is of, as
		// "dir.name", or "".
		var root func(e ast.Expr) string
		root = func(e ast.Expr) string {
			switch x := e.(type) {
			case *ast.Ident:
				if x.Obj == nil && own[x.Name] {
					return dir + "." + x.Name
				}
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Obj == nil {
					if p, ok := imports[id.Name]; ok {
						if vars[p][x.Sel.Name] {
							return p + "." + x.Sel.Name
						}
						return ""
					}
				}
				return root(x.X)
			case *ast.IndexExpr:
				return root(x.X)
			case *ast.StarExpr:
				return root(x.X)
			case *ast.ParenExpr:
				return root(x.X)
			}
			return ""
		}
		note := func(e ast.Expr) {
			if v := root(e); v != "" {
				if _, ok := testSwitchesAllowed[v]; !ok {
					found = append(found, fset.Position(e.Pos()).String()+": "+v)
				}
			}
		}
		for _, decl := range f.Decls {
			fd, isFunc := decl.(*ast.FuncDecl)
			if isFunc && fd.Recv == nil && fd.Name.Name == "init" {
				continue
			}
			if dir == "internal/vm" && f.Name.Name == "vm" && !(isFunc && fd.Name.Name == "newTestRuntime") {
				// The package's tests make every runtime by
				// newTestRuntime, which gives it its test's switches.
				ast.Inspect(decl, func(n ast.Node) bool {
					if c, ok := n.(*ast.CallExpr); ok {
						if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "New" && id.Obj == nil {
							found = append(found, fset.Position(c.Pos()).String()+": New, not newTestRuntime")
						}
					}
					return true
				})
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch s := n.(type) {
				case *ast.AssignStmt:
					if s.Tok != token.DEFINE {
						for _, l := range s.Lhs {
							note(l)
						}
					}
				case *ast.IncDecStmt:
					note(s.X)
				case *ast.CallExpr:
					if se, ok := s.Fun.(*ast.SelectorExpr); ok {
						switch se.Sel.Name {
						case "Store", "Add", "Swap", "CompareAndSwap", "Delete", "LoadOrStore":
							note(se.X)
						}
					}
				}
				return true
			})
		}
	}
	sort.Strings(found)
	for _, s := range found {
		t.Errorf("a test sets a package variable every runtime reads: %s", s)
	}
}

// collectorHelpers are the test files that may set the process's
// collector's percent: internal/vm's collector gate, whose holds and paces
// every other test goes through.
var collectorHelpers = map[string]bool{
	"internal/vm/collector_test.go": true,
}

// TestNoTestSetsTheCollectorPercent fails where a test calls
// debug.SetGCPercent but through the collector gate (internal/vm's
// collector_test.go: holdCollector, paceCollector): saved and restored by
// each of two overlapping tests on its own, the percent had been left off
// for the rest of the package, and its heavy tests' garbage ran the
// process out of memory.
func TestNoTestSetsTheCollectorPercent(t *testing.T) {
	t.Parallel()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != "." && (strings.HasPrefix(name, ".") || name == "testdata" || strings.HasPrefix(name, "zz")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") || collectorHelpers[filepath.ToSlash(path)] {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "SetGCPercent" {
					t.Errorf("%s: SetGCPercent outside the collector gate: hold or pace the collector through it (holdCollector, paceCollector)", fset.Position(c.Pos()))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
