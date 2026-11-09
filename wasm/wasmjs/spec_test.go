//go:build quickjs_wasm

package wasmjs_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/wasm/wasmjs"
)

// specResult is one subtest's outcome, as the harness records it.
type specResult struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

var metaScript = regexp.MustCompile(`(?m)^// META: script=(\S+)`)

// runSpecFile runs one of the WebAssembly JavaScript API tests, its helper
// scripts first, in a runtime of its own, and returns its subtests' results.
func runSpecFile(t *testing.T, root, file string, opts ...quickjs.Option) []specResult {
	t.Helper()
	rt := quickjs.New(opts...)
	defer rt.Close()
	if err := wasmjs.Install(rt); err != nil {
		t.Fatal(err)
	}
	harness, err := os.ReadFile(filepath.Join("testdata", "harness.js"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.EvalFile("harness.js", string(harness)); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range metaScript.FindAllStringSubmatch(string(src), -1) {
		path := m[1]
		if rest, ok := strings.CutPrefix(path, "/wasm/jsapi/"); ok {
			path = filepath.Join(root, filepath.FromSlash(rest))
		} else {
			path = filepath.Join(filepath.Dir(file), filepath.FromSlash(path))
		}
		helper, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rt.EvalFile(filepath.Base(path), string(helper)); err != nil {
			return []specResult{{Name: "(loading " + m[1] + ")", Status: "FAIL", Message: err.Error()}}
		}
	}
	var out []specResult
	if _, err := rt.EvalFile(filepath.Base(file), string(src)); err != nil {
		out = append(out, specResult{Name: "(file)", Status: "FAIL", Message: err.Error()})
	}
	if _, err := rt.Eval(`__wptRun()`); err != nil {
		out = append(out, specResult{Name: "(promise tests)", Status: "FAIL", Message: err.Error()})
	}
	if err := rt.RunJobs(); err != nil {
		out = append(out, specResult{Name: "(jobs)", Status: "FAIL", Message: err.Error()})
	}
	report, err := rt.Eval(`__wptReport()`)
	if err != nil {
		t.Fatal(err)
	}
	var results []specResult
	if err := json.Unmarshal([]byte(report.String()), &results); err != nil {
		t.Fatal(err)
	}
	return append(results, out...)
}

// specKnownFailures are the subtests expected to fail, by file and name,
// with why.
var specKnownFailures = map[string]string{
	// The global is an accessor until it is first read, as the runtime's
	// other globals built when first read (Intl, Temporal) are.
	"interface.any.js: WebAssembly: property descriptor": "a lazy global's descriptor is its accessor's",

	// Node 26 fails these the same way, with the same messages.
	"global/value-get-set.any.js: Calling setter without argument":              "V8 requires the setter's argument",
	"table/get-set.any.js: Arguments for anyfunc table set":                     "V8 converts an explicit undefined rather than taking it as missing",
	"table/grow.any.js: Grow with undefined argument":                           "V8 converts an explicit undefined rather than taking it as missing",
	"table/grow-memory64.any.js: Basic i64":                                     "the test calls nulls, which nothing defines",
	"table/grow-memory64.any.js: Reached maximum (i64)":                         "the test calls nulls, which nothing defines",
	"table/grow-memory64.any.js: Exceeded maximum (i64)":                        "the test calls nulls, which nothing defines",
	"limits.any.js: Validate tables over limit":                                 "V8 allows more tables than the test's limit",
	"limits.any.js: Compile tables over limit":                                  "V8 allows more tables than the test's limit",
	"limits.any.js: Async compile tables over limit":                            "V8 allows more tables than the test's limit",
	"limits.any.js: Validate memories over limit":                               "the test's limit of one memory predates multiple memories",
	"limits.any.js: Compile memories over limit":                                "the test's limit of one memory predates multiple memories",
	"limits.any.js: Async compile memories over limit":                          "the test's limit of one memory predates multiple memories",
	"limits.any.js: Validate initial table size beyond its dynamic limit":       "V8 refuses a table's initial size when it decodes the module",
	"limits.any.js: Compile initial table size beyond its dynamic limit":        "V8 refuses a table's initial size when it decodes the module",
	"limits.any.js: Async compile initial table size beyond its dynamic limit.": "V8 refuses a table's initial size when it decodes the module",
	"limits.any.js: Instantiate initial table size over limit":                  "V8 refuses a table's initial size when it decodes the module",
	"limits.any.js: Instantiate maximum table size over limit":                  "the test calls assertEquals, which nothing defines",
	"limits.any.js: Async instantiate maximum table size over limit":            "the test calls assertEquals, which nothing defines",
}

// TestWasmSpecJSAPI runs the WebAssembly specification's JavaScript API
// tests, test/js-api of a checkout of github.com/WebAssembly/spec named by
// WASM_SPEC_DIR, reporting every subtest.
func TestWasmSpecJSAPI(t *testing.T) {
	dir := os.Getenv("WASM_SPEC_DIR")
	if dir == "" {
		t.Skip("WASM_SPEC_DIR names no checkout of the WebAssembly specification")
	}
	root := filepath.Join(dir, "test", "js-api")
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".any.js") {
			files = append(files, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	pass, fail := 0, 0
	for _, file := range files {
		rel, _ := filepath.Rel(root, file)
		rel = filepath.ToSlash(rel)
		t.Run(rel, func(t *testing.T) {
			for _, r := range runSpecFile(t, root, file) {
				key := rel + ": " + r.Name
				switch {
				case r.Status == "PASS":
					pass++
					t.Logf("PASS %s", r.Name)
				case specKnownFailures[key] != "":
					fail++
					t.Logf("FAIL %s (expected: %s): %s", r.Name, specKnownFailures[key], r.Message)
				default:
					fail++
					t.Errorf("FAIL %s: %s", r.Name, r.Message)
				}
			}
		})
	}
	t.Logf("%d subtests passed, %d failed", pass, fail)
}
